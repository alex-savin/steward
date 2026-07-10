// Package dispatch serializes deployments per project so overlapping webhooks
// can never run two deployments for the same project at once. The webhook
// handler calls Enqueue, gets an immediate disposition (started / queued /
// rejected), and answers the HTTP request; the deployment itself runs on a
// background goroutine owned by this package.
package dispatch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"sync"
	"time"

	"github.com/alex-savin/steward/internal/config"
)

// Job is one requested deployment.
type Job struct {
	ID         string
	Project    config.Project
	Ref        string // e.g. refs/heads/main
	SHA        string // commit to deploy (push payload "after")
	Pusher     string
	Message    string // head commit subject line
	ReceivedAt time.Time
}

// Result is what the runner reports back after a deployment finishes.
type Result struct {
	// Mode is the concurrency mode from the project's manifest ("queue" or
	// "reject"). It governs how the NEXT overlapping webhook is handled —
	// the manifest lives in the repo, so we can only know it after a checkout.
	Mode string
}

// Progress is called by the runner as it enters each lifecycle step, so
// GET /status can distinguish a slow deployment from a wedged one.
type Progress func(step string)

// RunFunc executes one deployment end to end (the orchestrator's Run).
type RunFunc func(ctx context.Context, job Job, progress Progress) Result

// EnqueueStatus is the immediate disposition of a webhook.
type EnqueueStatus int

const (
	StatusStarted  EnqueueStatus = iota // nothing was running; started immediately
	StatusQueued                        // queued behind the running deployment
	StatusRejected                      // project busy and manifest says mode: reject
	// StatusQueueFull is retained for the HTTP layer's exhaustive switch.
	// Deprecated: unreachable since queue mode began coalescing — the queue
	// holds at most one pending job.
	StatusQueueFull
	StatusShutdown // agent is draining; not accepting work
)

func (s EnqueueStatus) String() string {
	switch s {
	case StatusStarted:
		return "started"
	case StatusQueued:
		return "queued"
	case StatusRejected:
		return "rejected"
	case StatusQueueFull:
		return "queue_full"
	case StatusShutdown:
		return "shutting_down"
	default:
		return "unknown"
	}
}

type Options struct {
	Run         RunFunc
	Log         *slog.Logger
	DefaultMode string // fallback when no manifest is available for a project

	// Deprecated: queue mode now coalesces to the newest pending job, so a
	// project's queue never holds more than one. Accepted and ignored so
	// existing configs keep loading.
	MaxQueue int

	// ResolveMode, when set, supplies a project's initial concurrency mode —
	// typically by reading deployment.mode from the manifest in the project's
	// persisted checkout, so a daemon restart doesn't forget "reject" and
	// queue an overlapping deployment. Return "" to fall back to DefaultMode.
	ResolveMode func(p config.Project) string

	// OnSuperseded, when set, is called for each queued job dropped because a
	// newer push for the same project arrived. The superseded SHA never
	// deploys, and something has to tell GitHub, or a CI job waiting on that
	// commit's status hangs until it times out. Called on its own goroutine —
	// it may block on the network — so it is not tracked by Shutdown.
	OnSuperseded func(dropped Job, by Job)
}

type Dispatcher struct {
	run          RunFunc
	log          *slog.Logger
	defaultMode  string
	maxQueue     int
	resolveMode  func(p config.Project) string
	onSuperseded func(dropped Job, by Job)

	baseCtx context.Context // parent of every deployment context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	mu       sync.Mutex
	closed   bool
	projects map[string]*projectState
}

// projectState is guarded by Dispatcher.mu.
type projectState struct {
	mode    string
	running *Job
	queue   []Job

	// step is the lifecycle step the running job is currently in, and
	// stepSince is when it entered that step.
	step      string
	stepSince time.Time
}

func New(opts Options) *Dispatcher {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.DefaultMode == "" {
		opts.DefaultMode = config.ModeQueue
	}
	if opts.MaxQueue <= 0 {
		opts.MaxQueue = 8
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Dispatcher{
		run:          opts.Run,
		log:          opts.Log,
		defaultMode:  opts.DefaultMode,
		maxQueue:     opts.MaxQueue,
		resolveMode:  opts.ResolveMode,
		onSuperseded: opts.OnSuperseded,
		baseCtx:      ctx,
		cancel:       cancel,
		projects:     make(map[string]*projectState),
	}
}

// NewID returns a short random job identifier.
func NewID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Enqueue hands a job to its project's worker. It never blocks: the decision
// is made under the lock and returned immediately, so the webhook handler can
// answer 202/409 right away.
func (d *Dispatcher) Enqueue(job Job) EnqueueStatus {
	// NOTE: no `defer Unlock` — the coalescing path releases the lock before
	// notifying about superseded jobs. Every return below unlocks explicitly.
	d.mu.Lock()

	if d.closed {
		d.mu.Unlock()
		return StatusShutdown
	}
	key := job.Project.Name
	p := d.projects[key]
	if p == nil {
		// First job for this project since startup: seed the mode from the
		// persisted checkout's manifest when possible (one small file read,
		// once per project), so mode survives daemon restarts.
		mode := d.defaultMode
		if d.resolveMode != nil {
			if m := d.resolveMode(job.Project); m == config.ModeQueue || m == config.ModeReject {
				mode = m
			}
		}
		p = &projectState{mode: mode}
		d.projects[key] = p
	}
	if p.running == nil {
		j := job
		p.running = &j
		d.wg.Add(1)
		go d.work(key, j)
		d.mu.Unlock()
		return StatusStarted
	}
	if p.mode == config.ModeReject {
		d.mu.Unlock()
		return StatusRejected
	}

	// Queue mode coalesces: only the newest push is worth deploying. The
	// checkout at the newest SHA already contains the intermediate trees, so
	// deploying a superseded commit would only burn a backup snapshot, a
	// rotation pass, and possibly a failed health check and rollback for a
	// commit the operator has already replaced.
	superseded := p.queue
	p.queue = []Job{job}
	d.mu.Unlock()

	for _, s := range superseded {
		d.log.Info("superseding queued deployment",
			"project", key, "dropped_job", s.ID, "dropped_sha", s.SHA, "by_sha", job.SHA)
		if d.onSuperseded != nil {
			// Off the webhook goroutine: the callback posts a commit status,
			// and the handler owes GitHub an immediate 202.
			go d.onSuperseded(s, job)
		}
	}
	return StatusQueued
}

// work runs job, then keeps draining the project's queue until it is empty.
func (d *Dispatcher) work(key string, job Job) {
	defer d.wg.Done()
	for {
		res := d.runOne(key, job)

		d.mu.Lock()
		p := d.projects[key]
		if res.Mode == config.ModeQueue || res.Mode == config.ModeReject {
			p.mode = res.Mode
		}
		if d.closed && len(p.queue) > 0 {
			d.log.Info("dropping queued deployments on shutdown", "project", key, "count", len(p.queue))
			p.queue = nil
		}
		p.step, p.stepSince = "", time.Time{}
		if len(p.queue) == 0 {
			p.running = nil
			d.mu.Unlock()
			return
		}
		job = p.queue[0]
		p.queue = p.queue[1:]
		p.running = &job
		d.mu.Unlock()
	}
}

// runOne isolates a single deployment so a panic in the pipeline downs the
// job, not the worker (which would strand the queue) or the whole agent.
func (d *Dispatcher) runOne(key string, job Job) (res Result) {
	defer func() {
		if r := recover(); r != nil {
			d.log.Error("deployment panicked", "project", job.Project.Name, "job", job.ID, "panic", r)
		}
	}()
	return d.run(d.baseCtx, job, d.progressFor(key, job.ID))
}

// progressFor returns the Progress callback for one job. It ignores updates
// from a job that is no longer the running one, so a straggling goroutine from
// a cancelled deployment cannot mislabel its successor.
func (d *Dispatcher) progressFor(key, jobID string) Progress {
	return func(step string) {
		d.mu.Lock()
		defer d.mu.Unlock()
		if p := d.projects[key]; p != nil && p.running != nil && p.running.ID == jobID {
			p.step = step
			p.stepSince = time.Now()
		}
	}
}

// JobInfo is the externally-visible summary of a job (GET /status).
type JobInfo struct {
	ID         string    `json:"id"`
	SHA        string    `json:"sha"`
	Ref        string    `json:"ref"`
	ReceivedAt time.Time `json:"received_at"`
	// Step and StepSince are set only for the running job: which lifecycle
	// step it is in and when it entered it. A deploy sitting in "verify" for
	// ten minutes is wedged; one in "backup" is merely slow.
	Step      string     `json:"step,omitempty"`
	StepSince *time.Time `json:"step_since,omitempty"`
}

type ProjectSnapshot struct {
	Mode    string    `json:"mode"`
	Running *JobInfo  `json:"running,omitempty"`
	Queued  []JobInfo `json:"queued,omitempty"`
}

func (d *Dispatcher) Snapshot() map[string]ProjectSnapshot {
	d.mu.Lock()
	defer d.mu.Unlock()

	snap := make(map[string]ProjectSnapshot, len(d.projects))
	for name, p := range d.projects {
		ps := ProjectSnapshot{Mode: p.mode}
		if p.running != nil {
			ps.Running = &JobInfo{ID: p.running.ID, SHA: p.running.SHA, Ref: p.running.Ref, ReceivedAt: p.running.ReceivedAt, Step: p.step}
			if !p.stepSince.IsZero() {
				since := p.stepSince
				ps.Running.StepSince = &since
			}
		}
		for _, j := range p.queue {
			ps.Queued = append(ps.Queued, JobInfo{ID: j.ID, SHA: j.SHA, Ref: j.Ref, ReceivedAt: j.ReceivedAt})
		}
		snap[name] = ps
	}
	return snap
}

// Shutdown stops accepting jobs and waits for running deployments to finish.
// If ctx expires first, in-flight deployments are cancelled (their exec'd
// commands are killed via CommandContext) and Shutdown waits for them to unwind.
func (d *Dispatcher) Shutdown(ctx context.Context) {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()

	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		d.log.Warn("shutdown grace expired; cancelling in-flight deployments")
		d.cancel()
		<-done
	}
	d.cancel()
}

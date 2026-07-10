package dispatch

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alex-savin/steward/internal/config"
)

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func job(sha string) Job {
	return Job{ID: NewID(), SHA: sha, Ref: "refs/heads/main", ReceivedAt: time.Now(),
		Project: config.Project{Name: "app", Repo: "o/app", Branch: "main"}}
}

// gate lets a test hold a deployment inside Run until it says otherwise.
type gate struct {
	entered chan string // SHA of each job that began running
	release chan struct{}
	ran     atomic.Int64
}

func newGate() *gate {
	return &gate{entered: make(chan string, 16), release: make(chan struct{})}
}

func (g *gate) run(_ context.Context, j Job, _ Progress) Result {
	g.ran.Add(1)
	g.entered <- j.SHA
	<-g.release
	return Result{}
}

// The headline fix: a burst of pushes during one slow deploy must collapse to
// the newest commit, not replay every superseded one.
func TestQueueCoalescesToNewestSHA(t *testing.T) {
	g := newGate()
	var superseded []string
	var mu sync.Mutex

	d := New(Options{Run: g.run, Log: quietLog(), DefaultMode: config.ModeQueue,
		OnSuperseded: func(dropped, by Job) {
			mu.Lock()
			superseded = append(superseded, dropped.SHA+"->"+by.SHA)
			mu.Unlock()
		}})

	if got := d.Enqueue(job("aaa")); got != StatusStarted {
		t.Fatalf("first enqueue = %v, want started", got)
	}
	<-g.entered // "aaa" is now running and blocked

	for _, sha := range []string{"bbb", "ccc", "ddd"} {
		if got := d.Enqueue(job(sha)); got != StatusQueued {
			t.Fatalf("enqueue %s = %v, want queued", sha, got)
		}
	}

	// Only the newest may be pending.
	snap := d.Snapshot()["app"]
	if len(snap.Queued) != 1 || snap.Queued[0].SHA != "ddd" {
		t.Fatalf("queue = %+v, want exactly [ddd]", snap.Queued)
	}

	close(g.release)
	if sha := <-g.entered; sha != "ddd" {
		t.Errorf("second deployment ran %s, want ddd", sha)
	}

	// Two jobs total: aaa and ddd. bbb and ccc never deploy.
	waitFor(t, func() bool { return g.ran.Load() == 2 })
	time.Sleep(50 * time.Millisecond)
	if n := g.ran.Load(); n != 2 {
		t.Errorf("ran %d deployments, want 2 (aaa, ddd)", n)
	}

	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(superseded) == 2 })
	mu.Lock()
	defer mu.Unlock()
	want := map[string]bool{"bbb->ccc": true, "ccc->ddd": true}
	for _, s := range superseded {
		if !want[s] {
			t.Errorf("unexpected supersede notice %q", s)
		}
	}
}

func TestRejectModeRefusesWhileBusy(t *testing.T) {
	g := newGate()
	d := New(Options{Run: g.run, Log: quietLog(), DefaultMode: config.ModeReject})

	if got := d.Enqueue(job("aaa")); got != StatusStarted {
		t.Fatalf("first = %v", got)
	}
	<-g.entered
	if got := d.Enqueue(job("bbb")); got != StatusRejected {
		t.Fatalf("second = %v, want rejected", got)
	}
	if q := d.Snapshot()["app"].Queued; len(q) != 0 {
		t.Errorf("reject mode must not queue, got %+v", q)
	}
	close(g.release)
}

// A superseded job must never be reported for the job that replaced it, and a
// project with an empty queue must not fire the callback at all.
func TestNoSupersedeNoticeWhenQueueEmpty(t *testing.T) {
	g := newGate()
	var calls atomic.Int64
	d := New(Options{Run: g.run, Log: quietLog(), DefaultMode: config.ModeQueue,
		OnSuperseded: func(Job, Job) { calls.Add(1) }})

	d.Enqueue(job("aaa"))
	<-g.entered
	d.Enqueue(job("bbb")) // queue was empty: nothing superseded
	close(g.release)

	time.Sleep(100 * time.Millisecond)
	if n := calls.Load(); n != 0 {
		t.Errorf("OnSuperseded called %d times, want 0", n)
	}
}

func TestProgressSurfacesInSnapshot(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	run := func(_ context.Context, _ Job, progress Progress) Result {
		progress("backup")
		close(entered)
		<-release
		return Result{}
	}
	d := New(Options{Run: run, Log: quietLog()})
	d.Enqueue(job("aaa"))
	<-entered

	waitFor(t, func() bool {
		r := d.Snapshot()["app"].Running
		return r != nil && r.Step == "backup"
	})
	r := d.Snapshot()["app"].Running
	if r.StepSince == nil || r.StepSince.IsZero() {
		t.Errorf("StepSince not set: %+v", r)
	}
	close(release)
}

// A panic must down the job, not the worker, and the queued job must still run.
func TestPanicDoesNotStrandTheQueue(t *testing.T) {
	var ran atomic.Int64
	started := make(chan string, 4)
	release := make(chan struct{})
	run := func(_ context.Context, j Job, _ Progress) Result {
		started <- j.SHA
		if j.SHA == "boom" {
			<-release
			panic("kaboom")
		}
		ran.Add(1)
		return Result{}
	}
	d := New(Options{Run: run, Log: quietLog(), DefaultMode: config.ModeQueue})
	d.Enqueue(job("boom"))
	<-started
	d.Enqueue(job("good"))
	close(release)

	waitFor(t, func() bool { return ran.Load() == 1 })
	if s := d.Snapshot()["app"]; s.Running != nil {
		t.Errorf("worker did not retire after the queue drained: %+v", s.Running)
	}
}

func TestShutdownRejectsNewWork(t *testing.T) {
	d := New(Options{Run: func(context.Context, Job, Progress) Result { return Result{} }, Log: quietLog()})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	d.Shutdown(ctx)
	if got := d.Enqueue(job("aaa")); got != StatusShutdown {
		t.Errorf("Enqueue after Shutdown = %v, want shutting_down", got)
	}
}

// Hammer Enqueue from many goroutines; `go test -race` is the assertion.
func TestConcurrentEnqueueIsRaceFree(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	run := func(_ context.Context, _ Job, progress Progress) Result {
		progress("deploy")
		once.Do(func() { <-release })
		return Result{}
	}
	d := New(Options{Run: run, Log: quietLog(), DefaultMode: config.ModeQueue})

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d.Enqueue(job("sha"))
			d.Snapshot()
		}(i)
	}
	close(release)
	wg.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d.Shutdown(ctx)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within 3s")
}

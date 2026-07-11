package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/alex-savin/steward/internal/config"
	"github.com/alex-savin/steward/internal/dispatch"
)

// maxWebhookBody caps payload reads. GitHub caps webhook payloads at 25 MB.
const maxWebhookBody = 25 << 20

// pushEvent is the subset of GitHub's push payload Steward acts on, plus two
// optional Steward extensions for the image-centric trigger (deploy_image +
// project). A real GitHub push carries neither, so behavior is unchanged for
// upstream webhooks.
type pushEvent struct {
	Ref        string `json:"ref"`
	After      string `json:"after"`
	Deleted    bool   `json:"deleted"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	HeadCommit *struct {
		Message string `json:"message"`
	} `json:"head_commit"`
	Pusher struct {
		Name string `json:"name"`
	} `json:"pusher"`

	// Steward extensions (image-centric deploys):
	DeployImage string `json:"deploy_image"` // container image the CD hook delivers
	Project     string `json:"project"`      // match a local project by name
}

// handleWebhook validates and dispatches a GitHub webhook. Everything here is
// synchronous and fast: signature check, payload parse, enqueue — then an
// immediate 202. The deployment itself runs on a dispatcher goroutine.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	log := s.log.With("remote", clientIP(r, s.cfg.TrustProxy))

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		http.Error(w, "payload too large or unreadable", http.StatusBadRequest)
		return
	}

	// HMAC-SHA256 first, in constant time, before touching the payload.
	if !validSignature(s.cfg.WebhookSecretBytes(), body, r.Header.Get("X-Hub-Signature-256")) {
		log.Warn("webhook rejected: bad or missing signature")
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	event := r.Header.Get("X-GitHub-Event")
	log = log.With("event", event, "delivery", r.Header.Get("X-GitHub-Delivery"))

	switch event {
	case "ping":
		writeJSON(w, http.StatusOK, map[string]string{"status": "pong"})
		return
	case "push":
		// handled below
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "reason": "unhandled event type"})
		return
	}

	var ev pushEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "malformed payload", http.StatusBadRequest)
		return
	}

	var project *config.Project
	job := dispatch.Job{ID: dispatch.NewID(), ReceivedAt: time.Now()}

	if ev.DeployImage != "" {
		// Image-centric trigger: a CD hook delivering a container to deploy.
		// Match by project name when given (local projects), else by repo+branch.
		if ev.Project != "" {
			project = s.cfg.FindProjectByName(ev.Project)
		} else {
			project = s.cfg.FindProject(ev.Repository.FullName, strings.TrimPrefix(ev.Ref, "refs/heads/"))
		}
		if project == nil {
			log.Info("image webhook ignored: no matching project", "project", ev.Project, "repo", ev.Repository.FullName)
			writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "reason": "no project configured"})
			return
		}
		job.Image = ev.DeployImage
		job.SHA = ev.After // optional; a git project may pin both
		job.Ref = ev.Ref
		job.Message = "deploy image " + ev.DeployImage
	} else {
		// Standard GitHub push.
		if ev.Deleted || ev.After == "" || ev.After == strings.Repeat("0", 40) {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "reason": "branch deletion"})
			return
		}
		branch := strings.TrimPrefix(ev.Ref, "refs/heads/")
		project = s.cfg.FindProject(ev.Repository.FullName, branch)
		if project == nil {
			log.Info("webhook ignored: no matching project", "repo", ev.Repository.FullName, "ref", ev.Ref)
			writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "reason": "no project configured for repo/branch"})
			return
		}
		if project.IsLocal() {
			log.Info("push ignored: local project deploys are image-centric", "project", project.Name)
			writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "reason": "local project requires a deploy_image trigger"})
			return
		}
		job.Ref = ev.Ref
		job.SHA = ev.After
		job.Pusher = ev.Pusher.Name
		if ev.HeadCommit != nil {
			job.Message = firstLine(ev.HeadCommit.Message)
		}
	}
	job.Project = *project

	// Validation done — answer immediately; the lifecycle runs in background.
	status := s.disp.Enqueue(job)
	log = log.With("job", job.ID, "project", project.Name, "sha", job.SHA)
	switch status {
	case dispatch.StatusStarted, dispatch.StatusQueued:
		log.Info("deployment accepted", "disposition", status.String())
		writeJSON(w, http.StatusAccepted, map[string]string{
			"status": status.String(),
			"job_id": job.ID,
		})
	case dispatch.StatusRejected:
		log.Warn("deployment rejected: another deployment is running (mode=reject)")
		writeJSON(w, http.StatusConflict, map[string]string{
			"status": "rejected",
			"reason": "deployment in progress and deployment.mode is reject",
		})
	case dispatch.StatusQueueFull:
		log.Warn("deployment rejected: queue full")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{
			"status": "rejected",
			"reason": "deployment queue is full",
		})
	default: // StatusShutdown
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "rejected",
			"reason": "agent is shutting down",
		})
	}
}

// validSignature checks GitHub's X-Hub-Signature-256 header ("sha256=<hex>")
// against the shared webhook secret, in constant time.
func validSignature(secret, body []byte, header string) bool {
	if len(secret) == 0 || header == "" {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(header))
}

// clientIP resolves the original client address for logging/audit. With
// Nginx's standard `proxy_set_header X-Forwarded-For
// $proxy_add_x_forwarded_for`, Nginx APPENDS the peer address it actually saw
// to whatever the client sent — so only the RIGHTMOST entry is trustworthy;
// the leftmost is attacker-controlled. Never used for authentication, which
// is the HMAC signature's job.
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
				return ip
			}
		}
		if rip := r.Header.Get("X-Real-Ip"); rip != "" {
			return rip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	if len(line) > 120 {
		line = line[:120]
	}
	return strings.TrimSpace(line)
}

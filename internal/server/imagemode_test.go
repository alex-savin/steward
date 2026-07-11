package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alex-savin/steward/internal/config"
	"github.com/alex-savin/steward/internal/dispatch"
)

// serverWithProjects builds a Server whose config has the given projects block.
func serverWithProjects(t *testing.T, projects string, run dispatch.RunFunc) *Server {
	t.Helper()
	dir := t.TempDir()
	body := fmt.Sprintf("listen: 127.0.0.1:0\ndata_dir: %s\nwebhook:\n  secret: %s\n%s", dir, secret, projects)
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	disp := dispatch.New(dispatch.Options{Run: run, Log: log, DefaultMode: config.ModeQueue})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		disp.Shutdown(ctx)
	})
	return New(cfg, log, disp)
}

// The image-centric trigger for a LOCAL project: matched by name, carries an
// image, needs no SHA.
func TestImageCentricTriggerByName(t *testing.T) {
	started := make(chan dispatch.Job, 1)
	release := make(chan struct{})
	defer close(release)
	s := serverWithProjects(t, "projects:\n  - name: firecrew\n    path: /opt/firecrew/src\n",
		func(_ context.Context, j dispatch.Job, _ dispatch.Progress) dispatch.Result {
			started <- j
			<-release
			return dispatch.Result{}
		})

	payload := `{"project":"firecrew","deploy_image":"ghcr.io/o/go-nttd:sha-abc"}`
	w := post(s, "push", payload, sign(secret, []byte(payload)))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", w.Code, w.Body)
	}
	select {
	case j := <-started:
		if j.Image != "ghcr.io/o/go-nttd:sha-abc" {
			t.Errorf("Image = %q", j.Image)
		}
		if j.SHA != "" {
			t.Errorf("SHA = %q, want empty for image-centric", j.SHA)
		}
		if !j.Project.IsLocal() || j.Project.Name != "firecrew" {
			t.Errorf("project = %+v", j.Project)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("deployment never started")
	}
}

// An image trigger can also target a git project (matched by repo).
func TestImageCentricTriggerGitProject(t *testing.T) {
	started := make(chan dispatch.Job, 1)
	release := make(chan struct{})
	defer close(release)
	s := serverWithProjects(t, "projects:\n  - repo: o/app\n    branch: main\n",
		func(_ context.Context, j dispatch.Job, _ dispatch.Progress) dispatch.Result {
			started <- j
			<-release
			return dispatch.Result{}
		})

	payload := `{"repository":{"full_name":"o/app"},"ref":"refs/heads/main","deploy_image":"ghcr.io/o/app:sha-x"}`
	w := post(s, "push", payload, sign(secret, []byte(payload)))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	if j := <-started; j.Image != "ghcr.io/o/app:sha-x" {
		t.Errorf("Image = %q", j.Image)
	}
}

// A plain push to a local project is rejected — local projects are image-driven.
func TestLocalProjectRejectsPush(t *testing.T) {
	ran := false
	s := serverWithProjects(t, "projects:\n  - name: firecrew\n    repo: o/app\n    path: /opt/firecrew/src\n",
		func(context.Context, dispatch.Job, dispatch.Progress) dispatch.Result {
			ran = true
			return dispatch.Result{}
		})

	body := pushPayload("9f2a1c3", "refs/heads/main") // repo o/app, plain push
	w := post(s, "push", body, sign(secret, []byte(body)))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "deploy_image") {
		t.Errorf("status=%d body=%s, want 200 + deploy_image hint", w.Code, w.Body)
	}
	if ran {
		t.Error("a plain push must not deploy a local project")
	}
}

// An image trigger for an unknown project is ignored, not an error.
func TestImageCentricUnknownProject(t *testing.T) {
	s := serverWithProjects(t, "projects:\n  - name: firecrew\n    path: /opt/firecrew\n",
		func(context.Context, dispatch.Job, dispatch.Progress) dispatch.Result { return dispatch.Result{} })
	payload := `{"project":"nope","deploy_image":"x:1"}`
	w := post(s, "push", payload, sign(secret, []byte(payload)))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "ignored") {
		t.Errorf("status=%d body=%s", w.Code, w.Body)
	}
}

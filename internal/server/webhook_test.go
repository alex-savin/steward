package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alex-savin/steward/internal/config"
	"github.com/alex-savin/steward/internal/dispatch"
)

const secret = "s3cr3t"

// validSignature is the only thing standing between the internet and arbitrary
// command execution on the deploy host. Test it directly.
func TestValidSignature(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/main"}`)
	good := sign(secret, body)

	cases := []struct {
		name   string
		secret string
		body   []byte
		header string
		want   bool
	}{
		{"correct signature", secret, body, good, true},
		{"wrong secret", "other", body, good, false},
		{"tampered body", secret, []byte(`{"ref":"refs/heads/evil"}`), good, false},
		{"missing header", secret, body, "", false},
		{"empty agent secret", "", body, good, false},
		{"no sha256= prefix", secret, body, strings.TrimPrefix(good, "sha256="), false},
		{"uppercase hex", secret, body, strings.ToUpper(good), false},
		{"sha1 style header", secret, body, "sha1=" + strings.TrimPrefix(good, "sha256="), false},
		{"garbage", secret, body, "sha256=zzzz", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := validSignature([]byte(tc.secret), tc.body, tc.header); got != tc.want {
				t.Errorf("validSignature = %v, want %v", got, tc.want)
			}
		})
	}
}

// Nginx appends the peer it saw, so only the RIGHTMOST entry is trustworthy.
func TestClientIP(t *testing.T) {
	cases := []struct {
		name       string
		trustProxy bool
		xff        string
		realIP     string
		remote     string
		want       string
	}{
		{"no proxy trust ignores headers", false, "1.2.3.4", "5.6.7.8", "10.0.0.1:1234", "10.0.0.1"},
		{"single hop", true, "203.0.113.7", "", "127.0.0.1:1", "203.0.113.7"},
		{"forged leftmost entry is ignored", true, "140.82.112.1, 203.0.113.7", "", "127.0.0.1:1", "203.0.113.7"},
		{"x-real-ip fallback", true, "", "203.0.113.9", "127.0.0.1:1", "203.0.113.9"},
		{"socket fallback", true, "", "", "192.168.1.5:9", "192.168.1.5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/webhook/github", nil)
			r.RemoteAddr = tc.remote
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			if tc.realIP != "" {
				r.Header.Set("X-Real-Ip", tc.realIP)
			}
			if got := clientIP(r, tc.trustProxy); got != tc.want {
				t.Errorf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// testServer wires a real Server to a real Dispatcher whose Run is `run`.
func testServer(t *testing.T, run dispatch.RunFunc) *Server {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	body := fmt.Sprintf("listen: 127.0.0.1:0\ndata_dir: %s\nwebhook:\n  secret: %s\nprojects:\n  - repo: o/app\n    branch: main\n", dir, secret)
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
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

func post(s *Server, event string, payload string, sig string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/webhook/github", strings.NewReader(payload))
	r.Header.Set("X-GitHub-Event", event)
	if sig != "" {
		r.Header.Set("X-Hub-Signature-256", sig)
	}
	w := httptest.NewRecorder()
	s.handleWebhook(w, r)
	return w
}

func pushPayload(sha, ref string) string {
	return fmt.Sprintf(`{"ref":%q,"after":%q,"repository":{"full_name":"o/app"},"pusher":{"name":"alex"}}`, ref, sha)
}

func TestHandleWebhookRejectsUnauthenticated(t *testing.T) {
	var ran int
	s := testServer(t, func(context.Context, dispatch.Job, dispatch.Progress) dispatch.Result {
		ran++
		return dispatch.Result{}
	})

	body := pushPayload("9f2a1c3", "refs/heads/main")
	for _, tc := range []struct{ name, sig string }{
		{"no signature", ""},
		{"wrong signature", sign("wrong", []byte(body))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if w := post(s, "push", body, tc.sig); w.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", w.Code)
			}
		})
	}
	if ran != 0 {
		t.Fatalf("an unauthenticated request started %d deployments", ran)
	}
}

func TestHandleWebhookAcceptsSignedPush(t *testing.T) {
	started := make(chan dispatch.Job, 1)
	release := make(chan struct{})
	s := testServer(t, func(_ context.Context, j dispatch.Job, _ dispatch.Progress) dispatch.Result {
		started <- j
		<-release
		return dispatch.Result{}
	})
	defer close(release)

	body := pushPayload("9f2a1c3", "refs/heads/main")
	w := post(s, "push", body, sign(secret, []byte(body)))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", w.Code, w.Body)
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["status"] != "started" || resp["job_id"] == "" {
		t.Errorf("body = %v", resp)
	}
	select {
	case j := <-started:
		if j.SHA != "9f2a1c3" || j.Project.Name != "app" {
			t.Errorf("job = %+v", j)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("deployment never started")
	}
}

// The 202 must be returned before the deployment finishes.
func TestHandleWebhookRespondsBeforeDeploymentCompletes(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	s := testServer(t, func(context.Context, dispatch.Job, dispatch.Progress) dispatch.Result {
		<-release // never completes during the request
		return dispatch.Result{}
	})

	body := pushPayload("9f2a1c3", "refs/heads/main")
	done := make(chan int, 1)
	go func() { done <- post(s, "push", body, sign(secret, []byte(body))).Code }()

	select {
	case code := <-done:
		if code != http.StatusAccepted {
			t.Errorf("status = %d, want 202", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler blocked on the deployment instead of returning 202")
	}
}

func TestHandleWebhookIgnoresAndRejects(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	s := testServer(t, func(context.Context, dispatch.Job, dispatch.Progress) dispatch.Result {
		<-block
		return dispatch.Result{}
	})

	t.Run("ping answers pong", func(t *testing.T) {
		b := `{"zen":"x"}`
		w := post(s, "ping", b, sign(secret, []byte(b)))
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "pong") {
			t.Errorf("%d %s", w.Code, w.Body)
		}
	})
	t.Run("unhandled event ignored", func(t *testing.T) {
		b := `{}`
		w := post(s, "issues", b, sign(secret, []byte(b)))
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "ignored") {
			t.Errorf("%d %s", w.Code, w.Body)
		}
	})
	t.Run("unknown branch ignored", func(t *testing.T) {
		b := pushPayload("9f2a1c3", "refs/heads/dev")
		w := post(s, "push", b, sign(secret, []byte(b)))
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "no project configured") {
			t.Errorf("%d %s", w.Code, w.Body)
		}
	})
	t.Run("tag push ignored", func(t *testing.T) {
		b := pushPayload("9f2a1c3", "refs/tags/v1.0.0")
		w := post(s, "push", b, sign(secret, []byte(b)))
		if w.Code != http.StatusOK {
			t.Errorf("%d %s", w.Code, w.Body)
		}
	})
	t.Run("branch deletion ignored", func(t *testing.T) {
		b := `{"ref":"refs/heads/main","after":"0000000000000000000000000000000000000000","deleted":true,"repository":{"full_name":"o/app"}}`
		w := post(s, "push", b, sign(secret, []byte(b)))
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "ignored") {
			t.Errorf("%d %s", w.Code, w.Body)
		}
	})
	t.Run("malformed json", func(t *testing.T) {
		b := `{"ref":`
		w := post(s, "push", b, sign(secret, []byte(b)))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%d, want 400", w.Code)
		}
	})
}

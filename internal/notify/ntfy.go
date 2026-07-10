package notify

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/alex-savin/steward/internal/config"
)

// NtfyMessage is one push notification.
type NtfyMessage struct {
	Title    string
	Body     string
	Priority string // ntfy priority name, e.g. "default", "high"
	Tags     string // comma-separated emoji shortcodes, e.g. "rocket"
}

// SendNtfy posts msg to the configured ntfy topic (a plain HTTP POST to
// <server>/<topic>, per the ntfy publish API).
func SendNtfy(ctx context.Context, cfg *config.Ntfy, msg NtfyMessage) error {
	if cfg == nil || cfg.Topic == "" {
		return nil
	}
	url := strings.TrimRight(cfg.Server, "/") + "/" + cfg.Topic
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(msg.Body))
	if err != nil {
		return err
	}
	req.Header.Set("Title", msg.Title)
	if msg.Priority != "" {
		req.Header.Set("Priority", msg.Priority)
	}
	if msg.Tags != "" {
		req.Header.Set("Tags", msg.Tags)
	}
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}

	hc := &http.Client{Timeout: 10 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("ntfy %s: %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

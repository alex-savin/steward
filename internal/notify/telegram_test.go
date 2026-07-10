package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alex-savin/steward/internal/config"
)

// testToken has the shape telego validates offline: <bot_id>:<35 chars>.
var testToken = "123456789:" + strings.Repeat("A", 35)

func TestNewTelegramDisabledWithoutToken(t *testing.T) {
	c, err := NewTelegram("", "")
	if err != nil {
		t.Fatalf("NewTelegram(\"\"): %v", err)
	}
	if c != nil {
		t.Fatal("expected a nil client when no token is configured")
	}
	// A nil client must be a safe no-op, like GitHubClient.
	if err := c.Send(context.Background(), &config.Telegram{ChatID: "1"}, TelegramMessage{}); err != nil {
		t.Errorf("nil client Send: %v", err)
	}
}

func TestNewTelegramRejectsMalformedToken(t *testing.T) {
	if _, err := NewTelegram("not-a-token", ""); err == nil {
		t.Fatal("expected an error for a malformed token")
	}
}

func TestParseChatID(t *testing.T) {
	cases := []struct {
		in       string
		wantID   int64
		wantUser string
		wantErr  bool
	}{
		{in: "123456", wantID: 123456},
		{in: "-1001234567890", wantID: -1001234567890}, // supergroup/channel
		{in: "@homelab_deploys", wantUser: "@homelab_deploys"},
		{in: "not a chat", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tc := range cases {
		got, err := parseChatID(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseChatID(%q): expected error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseChatID(%q): %v", tc.in, err)
			continue
		}
		if got.ID != tc.wantID || got.Username != tc.wantUser {
			t.Errorf("parseChatID(%q) = %+v", tc.in, got)
		}
	}
}

func TestRenderEscapesHTML(t *testing.T) {
	out := renderTelegram(TelegramMessage{
		Title:  `Deploy FAILED: app & <b>co</b>`,
		Lines:  []string{`step "verify": exit 1 <script>alert(1)</script>`},
		SHA:    "9f2a1c3",
		Failed: true,
	})
	// The injected markup must be inert...
	if strings.Contains(out, "<script>") || strings.Contains(out, "<b>co</b>") {
		t.Errorf("unescaped markup survived: %q", out)
	}
	for _, want := range []string{"&amp;", "&lt;script&gt;", "&lt;b&gt;co&lt;/b&gt;"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in %q", want, out)
		}
	}
	// ...while our own formatting tags remain.
	if !strings.HasPrefix(out, "🚨 <b>") {
		t.Errorf("expected failure icon and bold title, got %q", out)
	}
	// The icon tracks the Failed flag, not the wording of the title.
	ok := renderTelegram(TelegramMessage{Title: "Deployed app"})
	if !strings.HasPrefix(ok, "✅ <b>") {
		t.Errorf("expected success icon, got %q", ok)
	}
}

func TestRenderLinkifiesSHA(t *testing.T) {
	out := renderTelegram(TelegramMessage{
		Title: "Deployed app", SHA: "9f2a1c3",
		CommitURL: "https://github.com/o/r/commit/9f2a1c3",
	})
	if !strings.Contains(out, `<a href="https://github.com/o/r/commit/9f2a1c3">9f2a1c3</a>`) {
		t.Errorf("expected a commit link, got %q", out)
	}
	// Without a URL (GitHub Enterprise), the SHA is plain code, not a link.
	plain := renderTelegram(TelegramMessage{Title: "Deployed app", SHA: "9f2a1c3"})
	if strings.Contains(plain, "<a href") || !strings.Contains(plain, "<code>9f2a1c3</code>") {
		t.Errorf("expected a plain code SHA, got %q", plain)
	}
}

// Truncation must never split a tag or an entity, which would earn a 400.
func TestRenderTruncatesWithoutBreakingMarkup(t *testing.T) {
	cases := map[string]string{
		"long ascii":                           strings.Repeat("x", 50_000),
		"all ampersands (5x escape expansion)": strings.Repeat("&", 50_000),
		"multibyte":                            strings.Repeat("日", 50_000),
		"angle brackets":                       strings.Repeat("<>", 25_000),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			out := renderTelegram(TelegramMessage{Title: "t", Lines: []string{body}, SHA: "abc1234"})
			if len(out) > telegramMaxLen {
				t.Errorf("rendered %d bytes, over the %d limit", len(out), telegramMaxLen)
			}
			if !isBalancedMarkup(out) {
				t.Errorf("truncation broke markup: %q", tail(out))
			}
		})
	}
}

// isBalancedMarkup checks that the string ends outside any tag and does not end
// mid-entity — the two ways naive truncation corrupts Telegram HTML.
func isBalancedMarkup(s string) bool {
	if strings.Count(s, "<") != strings.Count(s, ">") {
		return false
	}
	// A trailing "&" or "&amp" (no ";") means an entity was cut.
	if i := strings.LastIndex(s, "&"); i >= 0 && !strings.Contains(s[i:], ";") {
		return false
	}
	return true
}

func tail(s string) string {
	if len(s) < 60 {
		return s
	}
	return "..." + s[len(s)-60:]
}

// End-to-end through telego against a fake Bot API server: this is the only way
// to confirm the params we set actually land on the wire.
func TestSendRoundTrip(t *testing.T) {
	type capture struct {
		path string
		body map[string]any
	}
	got := make(chan capture, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		got <- capture{path: r.URL.Path, body: body}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1,"date":1,"chat":{"id":1,"type":"private"}}}`)
	}))
	defer srv.Close()

	c, err := NewTelegram(testToken, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Telegram{ChatID: "-1001234567890", MessageThreadID: 42, Silent: true}
	err = c.Send(context.Background(), cfg, TelegramMessage{
		Title: "Deployed my-awesome-app (production)",
		Lines: []string{"alex/app in 42s"},
		SHA:   "9f2a1c3",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	cap := <-got
	if want := "/bot" + testToken + "/sendMessage"; cap.path != want {
		t.Errorf("path = %q, want %q", cap.path, want)
	}
	if cap.body["chat_id"] != "-1001234567890" && cap.body["chat_id"] != float64(-1001234567890) {
		t.Errorf("chat_id = %#v", cap.body["chat_id"])
	}
	if cap.body["parse_mode"] != "HTML" {
		t.Errorf("parse_mode = %#v, want HTML", cap.body["parse_mode"])
	}
	if cap.body["message_thread_id"] != float64(42) {
		t.Errorf("message_thread_id = %#v", cap.body["message_thread_id"])
	}
	if cap.body["disable_notification"] != true {
		t.Errorf("disable_notification = %#v", cap.body["disable_notification"])
	}
	lp, ok := cap.body["link_preview_options"].(map[string]any)
	if !ok || lp["is_disabled"] != true {
		t.Errorf("link_preview_options = %#v, want is_disabled", cap.body["link_preview_options"])
	}
	text, _ := cap.body["text"].(string)
	if !strings.Contains(text, "Deployed my-awesome-app") || !strings.Contains(text, "9f2a1c3") {
		t.Errorf("text = %q", text)
	}
}

// A failing Bot API must surface as an error, not a silent success.
func TestSendReportsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`)
	}))
	defer srv.Close()

	c, err := NewTelegram(testToken, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	err = c.Send(context.Background(), &config.Telegram{ChatID: "1"}, TelegramMessage{Title: "x"})
	if err == nil {
		t.Fatal("expected an error from a 400 response")
	}
	if !strings.Contains(err.Error(), "chat not found") {
		t.Errorf("error should carry the API description, got: %v", err)
	}
}

func TestSendSkipsWhenChatIDEmpty(t *testing.T) {
	c, err := NewTelegram(testToken, "http://127.0.0.1:0") // would fail if dialed
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Send(context.Background(), &config.Telegram{}, TelegramMessage{Title: "x"}); err != nil {
		t.Errorf("expected a no-op for an empty chat_id, got %v", err)
	}
	if err := c.Send(context.Background(), nil, TelegramMessage{Title: "x"}); err != nil {
		t.Errorf("expected a no-op for a nil config, got %v", err)
	}
}

// The Bot API embeds the token in the URL path, so a transport error is a
// *url.Error whose text contains the token. Our caller logs that error, so it
// must never carry the credential. Regression test for a real leak.
func TestSendRedactsTokenFromTransportErrors(t *testing.T) {
	// Port 1 is reserved and refuses connections: a plain, non-adversarial
	// transport failure, exactly like a Telegram outage.
	c, err := NewTelegram(testToken, "http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	err = c.Send(context.Background(), &config.Telegram{ChatID: "1"}, TelegramMessage{Title: "x"})
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("bot token leaked into the error: %v", err)
	}
	if !strings.Contains(err.Error(), "<redacted>") {
		t.Errorf("expected the token to be replaced with a placeholder, got: %v", err)
	}
	// The diagnostic value must survive redaction.
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("redaction destroyed the useful part of the error: %v", err)
	}
	// Unwrapping must not resurrect the token either.
	if u := errors.Unwrap(err); u != nil && strings.Contains(u.Error(), testToken) {
		t.Errorf("token reachable via Unwrap: %v", u)
	}
}

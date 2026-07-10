package notify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mymmrac/telego"

	"github.com/alex-savin/steward/internal/config"
)

// telegramMaxLen is Telegram's hard limit on message text, in UTF-16 code
// units. We truncate on runes well below it rather than risk a 400.
const telegramMaxLen = 4000

// TelegramClient posts deployment results to a Telegram chat. Like
// GitHubClient, a nil *TelegramClient is a valid no-op, so callers don't have
// to branch on whether a bot token is configured.
type TelegramClient struct {
	bot *telego.Bot
	// redactor scrubs the bot token out of error strings. The Bot API puts the
	// token in the URL path (…/bot<TOKEN>/sendMessage), so any transport error
	// is a *url.Error carrying the token in its message — and our caller logs
	// that error. Without this, a single "connection refused" publishes the
	// bot's credential to anyone who can read the agent's logs.
	redactor *strings.Replacer
}

// redact removes the bot token from a message before it reaches a log sink.
func (c *TelegramClient) redact(s string) string {
	if c == nil || c.redactor == nil {
		return s
	}
	return c.redactor.Replace(s)
}

// NewTelegram returns nil when token is empty (Telegram notifications
// disabled). The token is validated offline by telego; no network call is made
// here, so a bad token surfaces at startup rather than mid-deployment.
//
// telego's default logger writes to stdout; we route everything through slog,
// so it is discarded. The stdlib HTTP client is used instead of telego's
// fasthttp default to keep one HTTP stack in the binary.
func NewTelegram(token string, apiServer string) (*TelegramClient, error) {
	if token == "" {
		return nil, nil
	}
	opts := []telego.BotOption{
		telego.WithDiscardLogger(),
		telego.WithHTTPClient(&http.Client{Timeout: 15 * time.Second}),
	}
	if apiServer != "" {
		opts = append(opts, telego.WithAPIServer(apiServer))
	}
	redactor := strings.NewReplacer(token, "<redacted>")
	bot, err := telego.NewBot(token, opts...)
	if err != nil {
		return nil, fmt.Errorf("telegram bot: %s", redactor.Replace(err.Error()))
	}
	return &TelegramClient{bot: bot, redactor: redactor}, nil
}

// TelegramMessage is one deployment result, rendered as Telegram HTML.
type TelegramMessage struct {
	Title     string // e.g. "Deployed my-awesome-app (production)"
	Lines     []string
	Failed    bool
	CommitURL string // optional; linkifies the SHA line
	SHA       string
	Log       string // optional tail of the stack's output, rendered as <pre>
}

// Send posts msg to the chat named by cfg. A nil client or nil cfg is a no-op.
func (c *TelegramClient) Send(ctx context.Context, cfg *config.Telegram, msg TelegramMessage) error {
	if c == nil || cfg == nil || cfg.ChatID == "" {
		return nil
	}
	chatID, err := parseChatID(cfg.ChatID)
	if err != nil {
		return err
	}

	params := &telego.SendMessageParams{
		ChatID:              chatID,
		Text:                renderTelegram(msg),
		ParseMode:           telego.ModeHTML,
		MessageThreadID:     cfg.MessageThreadID,
		DisableNotification: cfg.Silent,
		// A deploy notification's commit link should not expand into a card.
		LinkPreviewOptions: &telego.LinkPreviewOptions{IsDisabled: true},
	}
	if _, err := c.bot.SendMessage(ctx, params); err != nil {
		// Deliberately not %w: the wrapped error's text carries the token, and
		// anything holding the original could re-expose it via Unwrap.
		return errors.New("telegram sendMessage: " + c.redact(err.Error()))
	}
	return nil
}

// parseChatID accepts a numeric chat ID ("-1001234567890", negative for
// groups/channels) or an "@channelusername".
func parseChatID(raw string) (telego.ChatID, error) {
	if strings.HasPrefix(raw, "@") {
		return telego.ChatID{Username: raw}, nil
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return telego.ChatID{}, fmt.Errorf("telegram chat_id %q is neither a number nor an @username", raw)
	}
	return telego.ChatID{ID: id}, nil
}

// renderTelegram builds the HTML body. Every interpolated value is escaped:
// commit messages and command output routinely contain <, > and &.
//
// Truncation happens on the *raw* text before escaping, never on the rendered
// string — cutting the output could slice through a tag or an &amp; entity and
// earn a 400 from Telegram. Escaping only grows text (& becomes &amp;), so we
// shrink the raw budget until the rendered message fits.
func renderTelegram(msg TelegramMessage) string {
	for budget := 1500; budget >= 16; budget /= 2 {
		if s := renderWithBudget(msg, budget); len(s) <= telegramMaxLen {
			return s
		}
	}
	// Pathological input (a body of nothing but '&'): fall back to the title.
	return truncateRunes(escapeTelegramHTML(msg.Title), telegramMaxLen)
}

// renderWithBudget renders with each raw line capped at budget runes.
func renderWithBudget(msg TelegramMessage, budget int) string {
	var b strings.Builder

	icon := "✅"
	if msg.Failed {
		icon = "🚨"
	}
	b.WriteString(icon + " <b>" + escapeTelegramHTML(truncateRunes(msg.Title, budget)) + "</b>")

	if msg.SHA != "" {
		b.WriteString("\n")
		sha := escapeTelegramHTML(truncateRunes(msg.SHA, budget))
		if msg.CommitURL != "" {
			b.WriteString(`<a href="` + escapeTelegramHTML(msg.CommitURL) + `">` + sha + `</a>`)
		} else {
			b.WriteString("<code>" + sha + "</code>")
		}
	}
	for _, line := range msg.Lines {
		b.WriteString("\n" + escapeTelegramHTML(truncateRunes(line, budget)))
	}
	if msg.Log != "" {
		// <pre> keeps the log monospaced; its contents are escaped like any
		// other interpolated value, so a log line cannot close the tag early.
		b.WriteString("\n<pre>" + escapeTelegramHTML(truncateRunes(msg.Log, budget)) + "</pre>")
	}
	return b.String()
}

// escapeTelegramHTML escapes exactly the three characters Telegram's HTML
// parse mode reserves. Using html.EscapeString would also rewrite quotes,
// which is unnecessary noise inside message text.
var telegramHTMLEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func escapeTelegramHTML(s string) string { return telegramHTMLEscaper.Replace(s) }

// truncateRunes cuts on a rune boundary so a multi-byte character is never
// split, which would make the payload invalid UTF-8.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

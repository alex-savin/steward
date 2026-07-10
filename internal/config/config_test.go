package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, extra string) string {
	t.Helper()
	dir := t.TempDir()
	body := "listen: 127.0.0.1:8484\n" +
		"data_dir: " + dir + "\n" +
		"webhook:\n  secret: shhh\n" +
		"projects:\n  - repo: alex-savin/app\n    branch: main\n" +
		extra
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const goodTelegramToken = "123456789:AAHfYSbcH1yQ4CmnDrPPBqYzBiZOhIQ0abc"

func TestTelegramTokenResolution(t *testing.T) {
	t.Run("inline token", func(t *testing.T) {
		c, err := Load(writeConfig(t, "telegram:\n  token: \""+goodTelegramToken+"\"\n"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.TelegramToken() != goodTelegramToken {
			t.Errorf("TelegramToken() = %q", c.TelegramToken())
		}
	})

	t.Run("token file, trailing newline trimmed", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "tg.token")
		if err := os.WriteFile(f, []byte(goodTelegramToken+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := Load(writeConfig(t, "telegram:\n  token_file: "+f+"\n"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.TelegramToken() != goodTelegramToken {
			t.Errorf("TelegramToken() = %q", c.TelegramToken())
		}
	})

	t.Run("env var fallback", func(t *testing.T) {
		t.Setenv("STEWARD_TELEGRAM_TOKEN", goodTelegramToken)
		c, err := Load(writeConfig(t, ""))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.TelegramToken() != goodTelegramToken {
			t.Errorf("TelegramToken() = %q", c.TelegramToken())
		}
	})

	t.Run("absent means disabled, not an error", func(t *testing.T) {
		c, err := Load(writeConfig(t, ""))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.TelegramToken() != "" {
			t.Errorf("expected no token, got %q", c.TelegramToken())
		}
	})
}

// A malformed token must fail at startup rather than silently dropping every
// notification — and the error must never echo the token itself.
func TestTelegramTokenMalformed(t *testing.T) {
	const secret = "not-a-valid-token-but-still-secret"
	_, err := Load(writeConfig(t, "telegram:\n  token: \""+secret+"\"\n"))
	if err == nil {
		t.Fatal("expected an error for a malformed telegram token")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error leaked the token: %v", err)
	}
}

// Package config holds both halves of Steward's configuration: the
// daemon's own Config (loaded once at startup) and the per-repository
// Manifest (.steward.yaml, re-read from the checkout on every deployment).
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the daemon's own configuration.
type Config struct {
	Listen        string   `yaml:"listen"`         // default 127.0.0.1:8484 (Nginx proxies to this)
	TrustProxy    bool     `yaml:"trust_proxy"`    // honor X-Forwarded-For / X-Real-Ip from Nginx
	DataDir       string   `yaml:"data_dir"`       // checkouts + state, default /var/lib/steward
	LockFile      string   `yaml:"lock_file"`      // single-instance lock, default <data_dir>/steward.lock
	ShutdownGrace Duration `yaml:"shutdown_grace"` // how long to let in-flight deploys finish, default 2m

	Webhook  WebhookConfig  `yaml:"webhook"`
	GitHub   GitHubConfig   `yaml:"github"`
	Telegram TelegramConfig `yaml:"telegram"`
	Defaults Defaults       `yaml:"defaults"`
	Projects []Project      `yaml:"projects"`

	secret        []byte
	token         string
	telegramToken string
}

type WebhookConfig struct {
	Secret     string `yaml:"secret"`      // inline secret (prefer secret_file)
	SecretFile string `yaml:"secret_file"` // file containing the GitHub webhook secret
	Path       string `yaml:"path"`        // URL path, default /webhook/github
}

type GitHubConfig struct {
	Token     string `yaml:"token"` // PAT with repo:status (prefer token_file)
	TokenFile string `yaml:"token_file"`
	APIURL    string `yaml:"api_url"` // default https://api.github.com
}

// TelegramConfig holds the bot credential. It is agent-side, not manifest-side:
// manifests are committed to application repos, and a bot token is a secret.
// Per-project routing (chat_id, topic) lives in the manifest.
type TelegramConfig struct {
	Token     string `yaml:"token"`      // prefer token_file
	TokenFile string `yaml:"token_file"` // file containing the bot token
	APIServer string `yaml:"api_server"` // override for a local Bot API server
}

// telegramTokenPattern is the shape Telegram issues and telego enforces
// (<bot_id>:<35 chars>). Checked here so a typo fails at startup rather than
// silently dropping every deployment notification.
var telegramTokenPattern = regexp.MustCompile(`^\d+:[\w-]{35}$`)

type Defaults struct {
	// DeployMode is used for a project until its manifest has been read once;
	// from then on the manifest's deployment.mode governs.
	DeployMode string `yaml:"deploy_mode"`
	MaxQueue   int    `yaml:"max_queue"` // queued deployments per project, default 8
}

// Project maps a GitHub repository/branch to a local working checkout.
type Project struct {
	Name     string `yaml:"name"`      // defaults to the repo name
	Repo     string `yaml:"repo"`      // "owner/name" exactly as GitHub reports it
	Branch   string `yaml:"branch"`    // deploy on pushes to this branch, default "main"
	CloneURL string `yaml:"clone_url"` // default https://github.com/<repo>.git
	Dir      string `yaml:"dir"`       // checkout dir, default <data_dir>/checkouts/<name>
	Manifest string `yaml:"manifest"`  // manifest path within the repo, default ".steward.yaml"
}

// Load reads and validates the daemon configuration.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := c.finalize(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) finalize() error {
	var errs []error

	if c.Listen == "" {
		c.Listen = "127.0.0.1:8484"
	}
	if c.DataDir == "" {
		c.DataDir = "/var/lib/steward"
	}
	c.DataDir = expandHome(c.DataDir)
	if c.LockFile == "" {
		c.LockFile = filepath.Join(c.DataDir, "steward.lock")
	}
	c.LockFile = expandHome(c.LockFile)
	if c.ShutdownGrace <= 0 {
		c.ShutdownGrace = Duration(2 * time.Minute)
	}
	if c.Webhook.Path == "" {
		c.Webhook.Path = "/webhook/github"
	}
	if !strings.HasPrefix(c.Webhook.Path, "/") {
		errs = append(errs, errors.New("webhook.path must start with /"))
	}

	secret, err := firstValue(c.Webhook.Secret, c.Webhook.SecretFile, "STEWARD_WEBHOOK_SECRET")
	if err != nil {
		errs = append(errs, fmt.Errorf("webhook secret: %w", err))
	} else if secret == "" {
		errs = append(errs, errors.New("webhook secret is required (webhook.secret, webhook.secret_file, or $STEWARD_WEBHOOK_SECRET)"))
	}
	c.secret = []byte(secret)

	token, err := firstValue(c.GitHub.Token, c.GitHub.TokenFile, "STEWARD_GITHUB_TOKEN")
	if err != nil {
		errs = append(errs, fmt.Errorf("github token: %w", err))
	}
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}
	c.token = token

	if c.GitHub.APIURL == "" {
		c.GitHub.APIURL = "https://api.github.com"
	}

	tgToken, err := firstValue(c.Telegram.Token, c.Telegram.TokenFile, "STEWARD_TELEGRAM_TOKEN")
	if err != nil {
		errs = append(errs, fmt.Errorf("telegram token: %w", err))
	} else if tgToken != "" && !telegramTokenPattern.MatchString(tgToken) {
		// Never echo the token itself into the error.
		errs = append(errs, errors.New(`telegram token is malformed (want "<bot_id>:<35 chars>")`))
	}
	c.telegramToken = tgToken

	switch c.Defaults.DeployMode {
	case "":
		c.Defaults.DeployMode = ModeQueue
	case ModeQueue, ModeReject:
	default:
		errs = append(errs, fmt.Errorf("defaults.deploy_mode must be %q or %q", ModeQueue, ModeReject))
	}
	if c.Defaults.MaxQueue <= 0 {
		c.Defaults.MaxQueue = 8
	}

	if len(c.Projects) == 0 {
		errs = append(errs, errors.New("at least one project must be configured"))
	}
	seen := map[string]bool{}
	seenNames := map[string]int{}
	seenDirs := map[string]int{}
	for i := range c.Projects {
		p := &c.Projects[i]
		owner, name, ok := strings.Cut(p.Repo, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			errs = append(errs, fmt.Errorf("projects[%d].repo must be \"owner/name\", got %q", i, p.Repo))
			continue
		}
		if p.Name == "" {
			p.Name = name
		}
		// The name becomes a directory and state-file component.
		if !filepath.IsLocal(p.Name) || strings.ContainsAny(p.Name, `/\`) {
			errs = append(errs, fmt.Errorf("projects[%d].name %q must be a plain name", i, p.Name))
			continue
		}
		if p.Branch == "" {
			p.Branch = "main"
		}
		if p.CloneURL == "" {
			p.CloneURL = "https://github.com/" + p.Repo + ".git"
		}
		if p.Dir == "" {
			p.Dir = filepath.Join(c.DataDir, "checkouts", p.Name)
		}
		p.Dir = expandHome(p.Dir)
		if p.Manifest == "" {
			p.Manifest = ".steward.yaml"
		}
		key := strings.ToLower(p.Repo) + "@" + p.Branch
		if seen[key] {
			errs = append(errs, fmt.Errorf("duplicate project for %s", key))
		}
		seen[key] = true

		// Name keys the checkout dir, state file, and dispatcher queue; a
		// collision (e.g. the same repo on two branches, both defaulting to
		// the repo name) would corrupt one project's rollback target with the
		// other's commits. Same for an explicit shared dir.
		if j, dup := seenNames[strings.ToLower(p.Name)]; dup {
			errs = append(errs, fmt.Errorf("projects[%d].name %q collides with projects[%d]; set distinct name: values", i, p.Name, j))
		} else {
			seenNames[strings.ToLower(p.Name)] = i
		}
		if j, dup := seenDirs[filepath.Clean(p.Dir)]; dup {
			errs = append(errs, fmt.Errorf("projects[%d].dir %q collides with projects[%d]", i, p.Dir, j))
		} else {
			seenDirs[filepath.Clean(p.Dir)] = i
		}
	}

	return errors.Join(errs...)
}

// firstValue resolves a credential from an inline value, a file, or an env var.
func firstValue(inline, file, envKey string) (string, error) {
	if inline != "" {
		return inline, nil
	}
	if file != "" {
		b, err := os.ReadFile(expandHome(file))
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	return os.Getenv(envKey), nil
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// WebhookSecretBytes returns the resolved webhook HMAC secret.
func (c *Config) WebhookSecretBytes() []byte { return c.secret }

// GitHubToken returns the resolved API token ("" disables commit statuses).
func (c *Config) GitHubToken() string { return c.token }

// TelegramToken returns the resolved bot token ("" disables Telegram).
func (c *Config) TelegramToken() string { return c.telegramToken }

// StateDir is where per-project state (last known-good SHA) lives.
func (c *Config) StateDir() string { return filepath.Join(c.DataDir, "state") }

// FindProject matches a webhook's repository + branch to a configured project.
func (c *Config) FindProject(repoFullName, branch string) *Project {
	for i := range c.Projects {
		p := &c.Projects[i]
		if strings.EqualFold(p.Repo, repoFullName) && p.Branch == branch {
			return p
		}
	}
	return nil
}

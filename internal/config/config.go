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

// Project maps a trigger to a deployment source. A git project clones a repo at
// the pushed SHA; a local project (Path set) reads its definition from a local
// directory and skips git entirely — the image-centric model, where CI ships a
// container and the hook says "deploy it" against a static local manifest.
type Project struct {
	Name     string `yaml:"name"`      // defaults to the repo name; required for a local project
	Repo     string `yaml:"repo"`      // "owner/name" match key; required unless Path is set
	Branch   string `yaml:"branch"`    // deploy on pushes to this branch, default "main"
	CloneURL string `yaml:"clone_url"` // default https://github.com/<repo>.git (git projects)
	Dir      string `yaml:"dir"`       // checkout dir, default <data_dir>/checkouts/<name>
	Manifest string `yaml:"manifest"`  // manifest path within the source, default ".steward.yaml"

	// Path makes this a LOCAL project: the manifest and unit/compose files are
	// read from this directory in place, with no git clone. It is the deploy
	// working directory. Mutually exclusive with clone-driven deploys.
	Path string `yaml:"path"`

	// Images is the allowlist of image repositories a deploy_image may name,
	// as repository prefixes ("registry.example.com/team/app"). Empty accepts
	// any image — the pre-allowlist behavior, warned about at startup.
	Images []string `yaml:"images"`

	// AllowAnyCommit turns off the branch ancestry check: by default a git
	// project only deploys a SHA reachable from origin/<branch>.
	AllowAnyCommit bool `yaml:"allow_any_commit"`
}

// IsLocal reports whether this project's definition comes from a local Path
// rather than a git checkout.
func (p *Project) IsLocal() bool { return p.Path != "" }

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

		// Name: from repo for git projects, explicit for local ones.
		if p.Name == "" && p.Repo != "" {
			if _, name, ok := strings.Cut(p.Repo, "/"); ok {
				p.Name = name
			}
		}
		if p.Branch == "" {
			p.Branch = "main"
		}
		if p.Manifest == "" {
			p.Manifest = ".steward.yaml"
		}

		if p.IsLocal() {
			// Local project: read the definition from Path in place, no clone.
			p.Path = expandHome(p.Path)
			if !filepath.IsAbs(p.Path) {
				errs = append(errs, fmt.Errorf("projects[%d].path %q must be absolute", i, p.Path))
			}
			if p.CloneURL != "" {
				errs = append(errs, fmt.Errorf("projects[%d]: path and clone_url are mutually exclusive", i))
			}
			if p.Name == "" {
				errs = append(errs, fmt.Errorf("projects[%d]: a local (path) project needs a name (or a repo to derive it)", i))
				continue
			}
			p.Dir = p.Path // deploy from the local dir directly
			if p.AllowAnyCommit {
				errs = append(errs, fmt.Errorf("projects[%d]: allow_any_commit only applies to git projects (no path)", i))
			}
		} else {
			// Git project: repo is required and must be "owner/name".
			owner, name, ok := strings.Cut(p.Repo, "/")
			if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
				errs = append(errs, fmt.Errorf("projects[%d].repo must be \"owner/name\" (or set path for a local project), got %q", i, p.Repo))
				continue
			}
			if p.CloneURL == "" {
				p.CloneURL = "https://github.com/" + p.Repo + ".git"
			}
			if p.Dir == "" {
				p.Dir = filepath.Join(c.DataDir, "checkouts", p.Name)
			}
			p.Dir = expandHome(p.Dir)
		}

		for j, img := range p.Images {
			if err := validImagePrefix(img); err != nil {
				errs = append(errs, fmt.Errorf("projects[%d].images[%d]: %w", i, j, err))
			}
			p.Images[j] = strings.TrimSuffix(img, "/")
		}

		// The name becomes a directory and state-file component.
		if !filepath.IsLocal(p.Name) || strings.ContainsAny(p.Name, `/\`) {
			errs = append(errs, fmt.Errorf("projects[%d].name %q must be a plain name", i, p.Name))
			continue
		}

		// Match key: repo@branch when a repo is set, else the project name
		// (local projects are dispatched by name).
		key := strings.ToLower(p.Name)
		if p.Repo != "" {
			key = strings.ToLower(p.Repo) + "@" + p.Branch
		}
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

// validImagePrefix checks an images: entry is a bare repository (prefix): no
// tag or digest, since those would make every new build fail the allowlist.
func validImagePrefix(s string) error {
	s = strings.TrimSuffix(s, "/")
	switch {
	case s == "":
		return errors.New("empty image repository")
	case strings.ContainsAny(s, " \t@") || strings.HasPrefix(s, "-"):
		return fmt.Errorf("%q is not an image repository", s)
	case imageRepository(s) != s:
		return fmt.Errorf("%q must be a repository without a tag", s)
	}
	return nil
}

// imageRepository strips the digest ("@sha256:…") and tag (":v1") from an image
// reference, leaving the repository. A colon before the last slash is a
// registry port ("host:5000/app"), not a tag.
func imageRepository(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref = ref[:i]
	}
	return ref
}

// AllowsImage reports whether a deploy_image reference is permitted for this
// project. With no images: allowlist every image is allowed. Otherwise the
// reference's repository must equal an entry or sit beneath it as a path:
// "reg/team/app" admits "reg/team/app:v1", "reg/team/app@sha256:…" and
// "reg/team/app/worker:v1", but not "reg/team/app-evil:v1". Matching is
// literal — no short-name or docker.io expansion — so list the exact
// registry host the CD pipeline pushes to.
func (p *Project) AllowsImage(ref string) bool {
	if len(p.Images) == 0 {
		return true
	}
	repo := imageRepository(ref)
	for _, prefix := range p.Images {
		if repo == prefix || strings.HasPrefix(repo, prefix+"/") {
			return true
		}
	}
	return false
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
		if p.Repo != "" && strings.EqualFold(p.Repo, repoFullName) && p.Branch == branch {
			return p
		}
	}
	return nil
}

// FindProjectByName matches an image-centric dispatch to a project by its name.
func (c *Config) FindProjectByName(name string) *Project {
	for i := range c.Projects {
		if strings.EqualFold(c.Projects[i].Name, name) {
			return &c.Projects[i]
		}
	}
	return nil
}

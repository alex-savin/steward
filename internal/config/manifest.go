package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Stack kinds supported by the deployment engine.
const (
	StackDockerCompose = "docker-compose"
	StackQuadlet       = "quadlet"
)

// Deployment concurrency modes: what happens when a webhook arrives while a
// deployment for the same project is already running.
const (
	ModeQueue  = "queue"
	ModeReject = "reject"
)

// Health check kinds.
const (
	HealthHTTP    = "http"
	HealthCommand = "command"
)

// Duration parses YAML durations like "10m" or "30s".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var raw string
	if err := node.Decode(&raw); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", raw, err)
	}
	*d = Duration(parsed)
	return nil
}

// Std returns the value as a stdlib time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Manifest is the per-repository deployment manifest, committed to the repo
// root as .steward.yaml. It drives the whole lifecycle:
// decrypt -> backup -> deploy -> verify -> (rollback) -> notify.
type Manifest struct {
	Project     string `yaml:"project"`
	Environment string `yaml:"environment"`
	Stack       string `yaml:"stack"` // "quadlet" | "docker-compose"

	Deployment    Deployment    `yaml:"deployment"`
	Secrets       Secrets       `yaml:"secrets"`
	Backup        Backup        `yaml:"backup"`
	Dependencies  []Dependency  `yaml:"dependencies"`
	HealthCheck   HealthCheck   `yaml:"health_check"`
	Notifications Notifications `yaml:"notifications"`

	// Optional driver tuning; sensible defaults apply when omitted.
	Compose ComposeOptions `yaml:"compose"`
	Quadlet QuadletOptions `yaml:"quadlet"`
}

// Deployment manages concurrency to prevent race conditions and bounds the
// whole lifecycle with one deadline.
type Deployment struct {
	Mode    string   `yaml:"mode"`    // queue | reject (while another deployment runs)
	Timeout Duration `yaml:"timeout"` // whole-lifecycle deadline, default 10m
}

// Secrets integrates encrypted secrets committed to Git (SOPS + age).
type Secrets struct {
	Provider string       `yaml:"provider"` // "sops" is the only supported provider
	KeyFile  string       `yaml:"key_file"` // age identity file on the deploy host
	Files    []SecretFile `yaml:"files"`
}

// SecretFile maps one encrypted file in the repo to its decrypted target,
// both relative to the repo root.
type SecretFile struct {
	Source      string `yaml:"source"`
	Destination string `yaml:"destination"`
}

// Backup declares the pre-deploy backup and its rotation policy.
type Backup struct {
	Retention Retention      `yaml:"retention"`
	Volumes   []VolumeBackup `yaml:"volumes"`
	Commands  []string       `yaml:"commands"`
}

type Retention struct {
	KeepLast int `yaml:"keep_last"` // snapshots to keep per destination, default 3
}

// VolumeBackup copies Source into a timestamped snapshot dir under Destination.
type VolumeBackup struct {
	Source      string `yaml:"source"`
	Destination string `yaml:"destination"`
}

// Dependency names a service/unit that needs special ordering. With
// start_first it is brought up before the rest of the stack.
type Dependency struct {
	Name       string `yaml:"name"`
	Type       string `yaml:"type"` // informational (e.g. "postgres")
	StartFirst bool   `yaml:"start_first"`
}

// HealthCheck verifies the deployment (Step 4). Timeout applies per attempt.
type HealthCheck struct {
	Type     string   `yaml:"type"`     // http | command | "" (skip)
	Endpoint string   `yaml:"endpoint"` // for http: URL to GET, 2xx passes
	Command  string   `yaml:"command"`  // for command: shell line, exit 0 passes
	Timeout  Duration `yaml:"timeout"`  // per attempt, default 30s
	Retries  int      `yaml:"retries"`  // default 3
}

type Notifications struct {
	Ntfy     *Ntfy     `yaml:"ntfy"`
	Telegram *Telegram `yaml:"telegram"`
}

// Telegram addresses a chat. The bot token is deliberately NOT here: this
// manifest is committed to the application repo, and a bot token is a
// credential. It lives in the agent's config (telegram.token / token_file /
// $STEWARD_TELEGRAM_TOKEN).
type Telegram struct {
	// ChatID is a numeric id ("-1001234567890" for groups/channels, negative)
	// or an "@channelusername". Quote it in YAML so it stays a string.
	ChatID string `yaml:"chat_id"`
	// MessageThreadID targets a topic in a forum supergroup.
	MessageThreadID int `yaml:"message_thread_id"`
	// Silent delivers without a notification sound.
	Silent bool `yaml:"silent"`
}

type Ntfy struct {
	Topic  string `yaml:"topic"`
	Server string `yaml:"server"` // default https://ntfy.sh
	Token  string `yaml:"token"`  // optional bearer token
}

// ComposeOptions tunes the docker-compose driver.
type ComposeOptions struct {
	File string `yaml:"file"` // default: first existing of compose.yaml/.yml, docker-compose.yml/.yaml
}

// QuadletOptions tunes the Podman Quadlet driver.
type QuadletOptions struct {
	Dir string `yaml:"dir"` // unit-file dir in the repo, default "quadlets"
}

// LoadManifest reads, defaults, and validates a manifest. Unknown YAML keys
// are rejected so typos fail loudly instead of silently disabling a feature.
func LoadManifest(path string) (*Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	defer f.Close()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", path, err)
	}
	m.applyDefaults()
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("invalid manifest %s: %w", path, err)
	}
	return &m, nil
}

func (m *Manifest) applyDefaults() {
	if m.Deployment.Mode == "" {
		m.Deployment.Mode = ModeQueue
	}
	if m.Deployment.Timeout <= 0 {
		m.Deployment.Timeout = Duration(10 * time.Minute)
	}
	if m.Backup.Retention.KeepLast == 0 {
		m.Backup.Retention.KeepLast = 3
	}
	if m.HealthCheck.Type != "" {
		if m.HealthCheck.Timeout <= 0 {
			m.HealthCheck.Timeout = Duration(30 * time.Second)
		}
		if m.HealthCheck.Retries <= 0 {
			m.HealthCheck.Retries = 3
		}
	}
	if m.Notifications.Ntfy != nil && m.Notifications.Ntfy.Server == "" {
		m.Notifications.Ntfy.Server = "https://ntfy.sh"
	}
	if m.Quadlet.Dir == "" {
		m.Quadlet.Dir = "quadlets"
	}
}

// Validate reports every problem at once rather than stopping at the first.
func (m *Manifest) Validate() error {
	var errs []error

	if m.Project == "" {
		errs = append(errs, errors.New("project is required"))
	}
	switch m.Stack {
	case StackDockerCompose, StackQuadlet:
	case "":
		errs = append(errs, errors.New("stack is required (quadlet or docker-compose)"))
	default:
		errs = append(errs, fmt.Errorf("unknown stack %q (want %q or %q)", m.Stack, StackQuadlet, StackDockerCompose))
	}
	switch m.Deployment.Mode {
	case ModeQueue, ModeReject:
	default:
		errs = append(errs, fmt.Errorf("deployment.mode must be %q or %q", ModeQueue, ModeReject))
	}

	if p := m.Secrets.Provider; p != "" && p != "sops" {
		errs = append(errs, fmt.Errorf("secrets.provider %q not supported (only \"sops\")", p))
	}
	if m.Secrets.Provider == "sops" && m.Secrets.KeyFile == "" {
		errs = append(errs, errors.New("secrets.key_file is required for the sops provider"))
	}
	for _, f := range m.Secrets.Files {
		if f.Source == "" || f.Destination == "" {
			errs = append(errs, errors.New("secrets.files entries need source and destination"))
			continue
		}
		// Both paths are joined to the checkout dir later; refuse anything
		// that could escape it (absolute paths, "..").
		if !filepath.IsLocal(f.Source) || !filepath.IsLocal(f.Destination) {
			errs = append(errs, fmt.Errorf("secrets file paths must stay inside the repo: %q -> %q", f.Source, f.Destination))
		}
	}

	if m.Backup.Retention.KeepLast < 1 {
		errs = append(errs, errors.New("backup.retention.keep_last must be >= 1"))
	}
	for _, v := range m.Backup.Volumes {
		if v.Source == "" || v.Destination == "" {
			errs = append(errs, errors.New("backup.volumes entries need source and destination"))
		}
	}

	switch m.HealthCheck.Type {
	case "":
	case HealthHTTP:
		if m.HealthCheck.Endpoint == "" {
			errs = append(errs, errors.New("health_check.endpoint is required for type http"))
		}
	case HealthCommand:
		if m.HealthCheck.Command == "" {
			errs = append(errs, errors.New("health_check.command is required for type command"))
		}
	default:
		errs = append(errs, fmt.Errorf("unknown health_check.type %q", m.HealthCheck.Type))
	}

	if n := m.Notifications.Ntfy; n != nil && n.Topic == "" {
		errs = append(errs, errors.New("notifications.ntfy.topic is required"))
	}
	if t := m.Notifications.Telegram; t != nil {
		switch {
		case t.ChatID == "":
			errs = append(errs, errors.New("notifications.telegram.chat_id is required"))
		case strings.HasPrefix(t.ChatID, "@"):
			if len(t.ChatID) < 2 {
				errs = append(errs, errors.New("notifications.telegram.chat_id: empty @username"))
			}
		default:
			if _, err := strconv.ParseInt(t.ChatID, 10, 64); err != nil {
				errs = append(errs, fmt.Errorf("notifications.telegram.chat_id %q must be a number or an @username", t.ChatID))
			}
		}
	}

	return errors.Join(errs...)
}

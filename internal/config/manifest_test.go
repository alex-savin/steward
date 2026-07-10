package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// exampleManifest is the reference .steward.yaml from the project spec.
const exampleManifest = `
project: my-awesome-app
environment: production
stack: quadlet # or docker-compose

deployment:
  mode: queue # Options: queue, reject (if another deployment is running)
  timeout: 10m

secrets:
  provider: sops
  key_file: /opt/steward/keys/age.txt
  files:
    - source: secrets/production.enc.env
      destination: .env

backup:
  retention:
    keep_last: 3
  volumes:
    - source: /var/lib/homelab/app_data
      destination: /opt/backups/my-awesome-app/
  commands:
    - docker exec db-1 pg_dump -U postgres myapp > /tmp/backup.sql

dependencies:
  - name: database
    type: postgres
    start_first: true

health_check:
  type: http
  endpoint: http://localhost:8080/healthz
  timeout: 30s
  retries: 3

notifications:
  ntfy:
    topic: homelab_deployments
    server: https://ntfy.mydomain.com
`

func writeManifest(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".steward.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadManifestExample(t *testing.T) {
	m, err := LoadManifest(writeManifest(t, exampleManifest))
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}

	if m.Project != "my-awesome-app" {
		t.Errorf("Project = %q", m.Project)
	}
	if m.Environment != "production" {
		t.Errorf("Environment = %q", m.Environment)
	}
	if m.Stack != StackQuadlet {
		t.Errorf("Stack = %q", m.Stack)
	}
	if m.Deployment.Mode != ModeQueue {
		t.Errorf("Deployment.Mode = %q", m.Deployment.Mode)
	}
	if m.Deployment.Timeout.Std() != 10*time.Minute {
		t.Errorf("Deployment.Timeout = %v", m.Deployment.Timeout.Std())
	}
	if m.Secrets.Provider != "sops" || m.Secrets.KeyFile != "/opt/steward/keys/age.txt" {
		t.Errorf("Secrets = %+v", m.Secrets)
	}
	if len(m.Secrets.Files) != 1 || m.Secrets.Files[0].Destination != ".env" {
		t.Errorf("Secrets.Files = %+v", m.Secrets.Files)
	}
	if m.Backup.Retention.KeepLast != 3 {
		t.Errorf("KeepLast = %d", m.Backup.Retention.KeepLast)
	}
	if len(m.Backup.Volumes) != 1 || m.Backup.Volumes[0].Source != "/var/lib/homelab/app_data" {
		t.Errorf("Backup.Volumes = %+v", m.Backup.Volumes)
	}
	if len(m.Backup.Commands) != 1 || !strings.Contains(m.Backup.Commands[0], "pg_dump") {
		t.Errorf("Backup.Commands = %+v", m.Backup.Commands)
	}
	if len(m.Dependencies) != 1 || !m.Dependencies[0].StartFirst || m.Dependencies[0].Name != "database" {
		t.Errorf("Dependencies = %+v", m.Dependencies)
	}
	if m.HealthCheck.Type != HealthHTTP || m.HealthCheck.Endpoint != "http://localhost:8080/healthz" {
		t.Errorf("HealthCheck = %+v", m.HealthCheck)
	}
	if m.HealthCheck.Timeout.Std() != 30*time.Second || m.HealthCheck.Retries != 3 {
		t.Errorf("HealthCheck timing = %+v", m.HealthCheck)
	}
	if m.Notifications.Ntfy == nil || m.Notifications.Ntfy.Topic != "homelab_deployments" {
		t.Errorf("Notifications = %+v", m.Notifications)
	}
	if m.Notifications.Ntfy.Server != "https://ntfy.mydomain.com" {
		t.Errorf("Ntfy.Server = %q", m.Notifications.Ntfy.Server)
	}
}

func TestManifestDefaults(t *testing.T) {
	m, err := LoadManifest(writeManifest(t, "project: x\nstack: docker-compose\n"))
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if m.Deployment.Mode != ModeQueue {
		t.Errorf("default Mode = %q, want queue", m.Deployment.Mode)
	}
	if m.Deployment.Timeout.Std() != 10*time.Minute {
		t.Errorf("default Timeout = %v, want 10m", m.Deployment.Timeout.Std())
	}
	if m.Backup.Retention.KeepLast != 3 {
		t.Errorf("default KeepLast = %d, want 3", m.Backup.Retention.KeepLast)
	}
	if m.Quadlet.Dir != "quadlets" {
		t.Errorf("default Quadlet.Dir = %q, want quadlets", m.Quadlet.Dir)
	}
}

func TestManifestRejectsBadValues(t *testing.T) {
	cases := map[string]string{
		"unknown stack":     "project: x\nstack: kubernetes\n",
		"missing project":   "stack: quadlet\n",
		"bad mode":          "project: x\nstack: quadlet\ndeployment:\n  mode: parallel\n",
		"escaping secret":   "project: x\nstack: quadlet\nsecrets:\n  provider: sops\n  key_file: /k\n  files:\n    - source: ../outside.env\n      destination: .env\n",
		"absolute secret":   "project: x\nstack: quadlet\nsecrets:\n  provider: sops\n  key_file: /k\n  files:\n    - source: s.enc.env\n      destination: /etc/passwd\n",
		"http w/o endpoint": "project: x\nstack: quadlet\nhealth_check:\n  type: http\n",
		"unknown yaml key":  "project: x\nstack: quadlet\ntypo_key: true\n",
	}
	for name, yaml := range cases {
		if _, err := LoadManifest(writeManifest(t, yaml)); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

func TestTelegramManifestValidation(t *testing.T) {
	base := "project: x\nstack: quadlet\nnotifications:\n  telegram:\n"
	valid := map[string]string{
		"numeric chat":  base + "    chat_id: \"-1001234567890\"\n",
		"username chat": base + "    chat_id: \"@homelab_deploys\"\n",
		"forum topic":   base + "    chat_id: \"123\"\n    message_thread_id: 42\n    silent: true\n",
	}
	for name, yaml := range valid {
		if _, err := LoadManifest(writeManifest(t, yaml)); err != nil {
			t.Errorf("%s: unexpected error: %v", name, err)
		}
	}

	invalid := map[string]string{
		"missing chat_id": base + "    silent: true\n",
		"bad chat_id":     base + "    chat_id: \"not a chat\"\n",
		"bare @":          base + "    chat_id: \"@\"\n",
		// A bot token must never live in a repo-committed manifest.
		"token in manifest": base + "    chat_id: \"1\"\n    token: \"123:abc\"\n",
	}
	for name, yaml := range invalid {
		if _, err := LoadManifest(writeManifest(t, yaml)); err == nil {
			t.Errorf("%s: expected an error, got nil", name)
		}
	}
}

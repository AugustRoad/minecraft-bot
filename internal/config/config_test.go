package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadDotEnvAndBotConfig(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	content := `
# comment line
DISCORD_TOKEN="tok_123"
DISCORD_ANNOUNCE_CHANNEL_ID=111222
GCP_PROJECT=demo-proj
GCP_ZONE=us-central1-a
MC_INSTANCE_NAME=mc-vm
WEBHOOK_SECRET=wh_secret
IDLE_TIMEOUT=20m
IDLE_WARNING_BEFORE=3m
`
	if err := os.WriteFile(envPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	keys := []string{
		"DISCORD_TOKEN", "DISCORD_ANNOUNCE_CHANNEL_ID", "GCP_PROJECT",
		"GCP_ZONE", "MC_INSTANCE_NAME", "WEBHOOK_SECRET", "IDLE_TIMEOUT", "IDLE_WARNING_BEFORE",
	}
	for _, k := range keys {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
	// Pre-set an env var to verify LoadDotEnv does not override existing environment values.
	t.Setenv("GCP_ZONE", "europe-west1-b")

	if err := LoadDotEnv(envPath); err != nil {
		t.Fatalf("LoadDotEnv error: %v", err)
	}

	cfg, err := LoadBotConfig()
	if err != nil {
		t.Fatalf("LoadBotConfig error: %v", err)
	}
	if cfg.DiscordToken != "tok_123" {
		t.Errorf("DiscordToken = %q, want tok_123", cfg.DiscordToken)
	}
	if cfg.GCPZone != "europe-west1-b" {
		t.Errorf("GCPZone = %q, want europe-west1-b (existing env should win)", cfg.GCPZone)
	}
	if cfg.IdleTimeout != 20*time.Minute || cfg.IdleWarningBefore != 3*time.Minute {
		t.Errorf("unexpected timers: idle=%v warn=%v", cfg.IdleTimeout, cfg.IdleWarningBefore)
	}
}

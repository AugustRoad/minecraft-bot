package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// BotConfig holds runtime configuration for the 24/7 Discord controller (mc-bot).
type BotConfig struct {
	DiscordToken      string
	DiscordGuildID    string
	AnnounceChannelID string
	ChatChannelID     string

	GCPProject     string
	GCPZone        string
	MCInstanceName string

	MCPort       int
	RCONPort     int
	RCONPassword string

	WebhookListenAddr string
	WebhookSecret     string

	StartupGracePeriod time.Duration
	IdleTimeout        time.Duration
	IdleWarningBefore  time.Duration
	PollInterval       time.Duration
	BootPollInterval   time.Duration
	BootTimeout        time.Duration
	RCONStopTimeout    time.Duration
}

// AgentConfig holds runtime configuration for the Minecraft VM companion agent (mc-agent).
type AgentConfig struct {
	LogPath       string
	BotWebhookURL string
	WebhookSecret string
	PollInterval  time.Duration
	SendBootEvent bool
}

// LoadDotEnv loads KEY=VALUE pairs from path into the process environment if the file exists.
// Existing environment variables are never overwritten.
func LoadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		if key != "" && os.Getenv(key) == "" {
			_ = os.Setenv(key, val)
		}
	}
	return scanner.Err()
}

// LoadBotConfig reads and validates BotConfig from environment variables.
func LoadBotConfig() (BotConfig, error) {
	cfg := BotConfig{
		DiscordToken:       strings.TrimSpace(os.Getenv("DISCORD_TOKEN")),
		DiscordGuildID:     strings.TrimSpace(os.Getenv("DISCORD_GUILD_ID")),
		AnnounceChannelID:  strings.TrimSpace(os.Getenv("DISCORD_ANNOUNCE_CHANNEL_ID")),
		ChatChannelID:      strings.TrimSpace(os.Getenv("DISCORD_CHAT_CHANNEL_ID")),
		GCPProject:         strings.TrimSpace(os.Getenv("GCP_PROJECT")),
		GCPZone:            strings.TrimSpace(os.Getenv("GCP_ZONE")),
		MCInstanceName:     strings.TrimSpace(os.Getenv("MC_INSTANCE_NAME")),
		MCPort:             envInt("MC_PORT", 25565),
		RCONPort:           envInt("MC_RCON_PORT", 25575),
		RCONPassword:       os.Getenv("MC_RCON_PASSWORD"),
		WebhookListenAddr:  envString("WEBHOOK_LISTEN_ADDR", ":8080"),
		WebhookSecret:      os.Getenv("WEBHOOK_SECRET"),
		StartupGracePeriod: envDuration("STARTUP_GRACE_PERIOD", 10*time.Minute),
		IdleTimeout:        envDuration("IDLE_TIMEOUT", 15*time.Minute),
		IdleWarningBefore:  envDuration("IDLE_WARNING_BEFORE", 2*time.Minute),
		PollInterval:       envDuration("POLL_INTERVAL", 60*time.Second),
		BootPollInterval:   envDuration("BOOT_POLL_INTERVAL", 3*time.Second),
		BootTimeout:        envDuration("BOOT_TIMEOUT", 3*time.Minute),
		RCONStopTimeout:    envDuration("RCON_STOP_TIMEOUT", 30*time.Second),
	}

	var missing []string
	if cfg.DiscordToken == "" {
		missing = append(missing, "DISCORD_TOKEN")
	}
	if cfg.AnnounceChannelID == "" {
		missing = append(missing, "DISCORD_ANNOUNCE_CHANNEL_ID")
	}
	if cfg.GCPProject == "" {
		missing = append(missing, "GCP_PROJECT")
	}
	if cfg.GCPZone == "" {
		missing = append(missing, "GCP_ZONE")
	}
	if cfg.MCInstanceName == "" {
		missing = append(missing, "MC_INSTANCE_NAME")
	}
	if cfg.WebhookSecret == "" {
		missing = append(missing, "WEBHOOK_SECRET")
	}
	if len(missing) > 0 {
		return cfg, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}

	if cfg.IdleTimeout > 0 && cfg.IdleWarningBefore >= cfg.IdleTimeout {
		cfg.IdleWarningBefore = cfg.IdleTimeout / 5
	}

	return cfg, nil
}

// LoadAgentConfig reads and validates AgentConfig from environment variables.
func LoadAgentConfig() (AgentConfig, error) {
	cfg := AgentConfig{
		LogPath:       strings.TrimSpace(os.Getenv("MC_LOG_PATH")),
		BotWebhookURL: strings.TrimSpace(os.Getenv("BOT_WEBHOOK_URL")),
		WebhookSecret: os.Getenv("WEBHOOK_SECRET"),
		PollInterval:  envDuration("AGENT_POLL_INTERVAL", 250*time.Millisecond),
		SendBootEvent: envBool("AGENT_SEND_BOOT_EVENT", true),
	}

	var missing []string
	if cfg.LogPath == "" {
		missing = append(missing, "MC_LOG_PATH")
	}
	if cfg.BotWebhookURL == "" {
		missing = append(missing, "BOT_WEBHOOK_URL")
	}
	if cfg.WebhookSecret == "" {
		missing = append(missing, "WEBHOOK_SECRET")
	}
	if len(missing) > 0 {
		return cfg, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}

	return cfg, nil
}

func envString(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return parsed
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return parsed
}

func envBool(key string, fallback bool) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if v == "" {
		return fallback
	}
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

package config

import (
	"bufio"
	"errors"
	"fmt"
	"net/url"
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
// Existing environment variables (even if explicitly set to empty) are never overwritten.
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
		val = parseDotEnvValue(val)
		if key != "" {
			if _, exists := os.LookupEnv(key); !exists {
				_ = os.Setenv(key, val)
			}
		}
	}
	return scanner.Err()
}

func parseDotEnvValue(raw string) string {
	val := strings.TrimSpace(raw)
	if len(val) == 0 {
		return ""
	}
	if val[0] == '"' || val[0] == '\'' {
		quote := val[0]
		if end := strings.IndexByte(val[1:], quote); end >= 0 {
			return val[1 : 1+end]
		}
		return val[1:]
	}
	// Strip unquoted inline comments preceded by whitespace.
	for i := 0; i < len(val); i++ {
		if val[i] == '#' && (i == 0 || val[i-1] == ' ' || val[i-1] == '\t') {
			val = val[:i]
			break
		}
	}
	return strings.TrimSpace(val)
}

// LoadBotConfig reads and validates BotConfig from environment variables.
func LoadBotConfig() (BotConfig, error) {
	var errs []string

	mcPort, err := envInt("MC_PORT", 25565, 1, 65535)
	if err != nil {
		errs = append(errs, err.Error())
	}
	rconPort, err := envInt("MC_RCON_PORT", 25575, 1, 65535)
	if err != nil {
		errs = append(errs, err.Error())
	}
	startupGrace, err := envDuration("STARTUP_GRACE_PERIOD", 10*time.Minute, true)
	if err != nil {
		errs = append(errs, err.Error())
	}
	idleTimeout, err := envDuration("IDLE_TIMEOUT", 15*time.Minute, true)
	if err != nil {
		errs = append(errs, err.Error())
	}
	idleWarn, err := envDuration("IDLE_WARNING_BEFORE", 2*time.Minute, true)
	if err != nil {
		errs = append(errs, err.Error())
	}
	pollInterval, err := envDuration("POLL_INTERVAL", 60*time.Second, false)
	if err != nil {
		errs = append(errs, err.Error())
	}
	bootPoll, err := envDuration("BOOT_POLL_INTERVAL", 3*time.Second, false)
	if err != nil {
		errs = append(errs, err.Error())
	}
	bootTimeout, err := envDuration("BOOT_TIMEOUT", 3*time.Minute, false)
	if err != nil {
		errs = append(errs, err.Error())
	}
	rconStopTimeout, err := envDuration("RCON_STOP_TIMEOUT", 30*time.Second, false)
	if err != nil {
		errs = append(errs, err.Error())
	}

	cfg := BotConfig{
		DiscordToken:       strings.TrimSpace(os.Getenv("DISCORD_TOKEN")),
		DiscordGuildID:     strings.TrimSpace(os.Getenv("DISCORD_GUILD_ID")),
		AnnounceChannelID:  strings.TrimSpace(os.Getenv("DISCORD_ANNOUNCE_CHANNEL_ID")),
		ChatChannelID:      strings.TrimSpace(os.Getenv("DISCORD_CHAT_CHANNEL_ID")),
		GCPProject:         strings.TrimSpace(os.Getenv("GCP_PROJECT")),
		GCPZone:            strings.TrimSpace(os.Getenv("GCP_ZONE")),
		MCInstanceName:     strings.TrimSpace(os.Getenv("MC_INSTANCE_NAME")),
		MCPort:             mcPort,
		RCONPort:           rconPort,
		RCONPassword:       os.Getenv("MC_RCON_PASSWORD"),
		WebhookListenAddr:  envString("WEBHOOK_LISTEN_ADDR", ":8080"),
		WebhookSecret:      os.Getenv("WEBHOOK_SECRET"),
		StartupGracePeriod: startupGrace,
		IdleTimeout:        idleTimeout,
		IdleWarningBefore:  idleWarn,
		PollInterval:       pollInterval,
		BootPollInterval:   bootPoll,
		BootTimeout:        bootTimeout,
		RCONStopTimeout:    rconStopTimeout,
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
		errs = append(errs, fmt.Sprintf("missing required environment variables: %s", strings.Join(missing, ", ")))
	}
	if len(errs) > 0 {
		return cfg, errors.New(strings.Join(errs, "; "))
	}

	if cfg.IdleTimeout > 0 && cfg.IdleWarningBefore >= cfg.IdleTimeout {
		cfg.IdleWarningBefore = cfg.IdleTimeout / 5
	}

	return cfg, nil
}

// LoadAgentConfig reads and validates AgentConfig from environment variables.
func LoadAgentConfig() (AgentConfig, error) {
	var errs []string

	pollInterval, err := envDuration("AGENT_POLL_INTERVAL", 250*time.Millisecond, false)
	if err != nil {
		errs = append(errs, err.Error())
	}
	sendBoot, err := envBool("AGENT_SEND_BOOT_EVENT", true)
	if err != nil {
		errs = append(errs, err.Error())
	}

	cfg := AgentConfig{
		LogPath:       strings.TrimSpace(os.Getenv("MC_LOG_PATH")),
		BotWebhookURL: strings.TrimSpace(os.Getenv("BOT_WEBHOOK_URL")),
		WebhookSecret: os.Getenv("WEBHOOK_SECRET"),
		PollInterval:  pollInterval,
		SendBootEvent: sendBoot,
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
		errs = append(errs, fmt.Sprintf("missing required environment variables: %s", strings.Join(missing, ", ")))
	}

	if cfg.BotWebhookURL != "" {
		u, err := url.Parse(cfg.BotWebhookURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Sprintf("invalid BOT_WEBHOOK_URL %q: must be an http/https URL with host", cfg.BotWebhookURL))
		}
	}

	if len(errs) > 0 {
		return cfg, errors.New(strings.Join(errs, "; "))
	}

	return cfg, nil
}

func envString(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback, min, max int) (int, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(v)
	if err != nil {
		return fallback, fmt.Errorf("invalid integer for %s=%q", key, v)
	}
	if parsed < min || parsed > max {
		return fallback, fmt.Errorf("%s=%d out of range [%d..%d]", key, parsed, min, max)
	}
	return parsed, nil
}

func envDuration(key string, fallback time.Duration, allowZero bool) (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(v)
	if err != nil {
		return fallback, fmt.Errorf("invalid duration for %s=%q", key, v)
	}
	if parsed < 0 || (!allowZero && parsed == 0) {
		return fallback, fmt.Errorf("%s=%q must be positive", key, v)
	}
	return parsed, nil
}

func envBool(key string, fallback bool) (bool, error) {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if v == "" {
		return fallback, nil
	}
	switch v {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return fallback, fmt.Errorf("invalid boolean for %s=%q", key, v)
	}
}

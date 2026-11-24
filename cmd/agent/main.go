package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/AugustRoad/minecraft-bot/internal/config"
	"github.com/AugustRoad/minecraft-bot/internal/events"
	"github.com/AugustRoad/minecraft-bot/internal/logtail"
	"github.com/AugustRoad/minecraft-bot/internal/webhook"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := config.LoadDotEnv(".env"); err != nil {
		logger.Warn("failed to read .env file", "err", err)
	}

	cfg, err := config.LoadAgentConfig()
	if err != nil {
		logger.Error("invalid agent configuration", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	httpClient := &http.Client{Timeout: 8 * time.Second}
	sender := func(ev events.Event) {
		if err := sendEventWithRetry(ctx, httpClient, cfg.BotWebhookURL, cfg.WebhookSecret, ev); err != nil {
			logger.Warn("failed to deliver event to mc-bot", "type", ev.Type, "err", err)
		}
	}

	logger.Info("mc-agent starting", "log_path", cfg.LogPath, "webhook_url", cfg.BotWebhookURL)

	// Announce VM boot immediately so even external/console starts trigger a fresh IP post in Discord.
	if cfg.SendBootEvent {
		go sender(events.Event{
			Type:      events.TypeVMBoot,
			Timestamp: time.Now().UTC(),
		})
	}

	err = logtail.TailFile(ctx, cfg.LogPath, cfg.PollInterval, logger, func(line string) {
		ev, ok := logtail.ParseLine(line, time.Now().UTC())
		if !ok {
			return
		}
		logger.Info("parsed minecraft event", "type", ev.Type, "player", ev.Player)
		sender(ev)
	})

	if err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("log tailer exited with error", "err", err)
		os.Exit(1)
	}
	logger.Info("mc-agent stopped cleanly")
}

func sendEventWithRetry(ctx context.Context, client *http.Client, url, secret string, ev events.Event) error {
	payload, err := json.Marshal(ev)
	if err != nil {
		return err
	}

	backoff := 500 * time.Millisecond
	var lastErr error

	for attempt := 1; attempt <= 4; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(webhook.HeaderSecret, secret)

		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
			lastErr = fmt.Errorf("webhook returned status %d", resp.StatusCode)
			if resp.StatusCode >= 400 && resp.StatusCode < 500 {
				return lastErr // Don't retry client/auth errors
			}
		} else {
			lastErr = err
		}

		if attempt == 4 {
			break
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
			backoff *= 2
		}
	}

	return lastErr
}

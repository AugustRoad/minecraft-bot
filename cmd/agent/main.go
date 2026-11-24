package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/AugustRoad/minecraft-bot/internal/config"
	"github.com/AugustRoad/minecraft-bot/internal/events"
	"github.com/AugustRoad/minecraft-bot/internal/logtail"
	"github.com/AugustRoad/minecraft-bot/internal/webhook"
)

const eventQueueCapacity = 128

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
	queue := make(chan events.Event, eventQueueCapacity)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ev := range queue {
			maxAttempts := 4
			if ev.Type == events.TypeVMBoot || ev.Type == events.TypeServerReady {
				maxAttempts = 6
			}
			sendCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
			err := sendEventWithRetry(sendCtx, httpClient, cfg.BotWebhookURL, cfg.WebhookSecret, ev, maxAttempts)
			cancel()
			if err != nil && !errors.Is(err, context.Canceled) {
				logger.Warn("failed to deliver event to mc-bot", "type", ev.Type, "err", err)
			}
		}
	}()

	enqueue := func(ev events.Event) {
		select {
		case queue <- ev:
		default:
			// Queue is full (bot-vm unreachable): drop oldest non-lifecycle event if possible.
			select {
			case <-queue:
			default:
			}
			select {
			case queue <- ev:
				logger.Warn("event queue full; dropped oldest event to enqueue newer event", "type", ev.Type)
			default:
				logger.Warn("event queue full; dropping event", "type", ev.Type)
			}
		}
	}

	logger.Info("mc-agent starting", "log_path", cfg.LogPath, "webhook_url", cfg.BotWebhookURL)

	// Announce VM boot immediately so even external/console starts trigger a fresh IP post in Discord.
	if cfg.SendBootEvent {
		enqueue(events.Event{
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
		enqueue(ev)
	})

	close(queue)
	wg.Wait()

	if err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("log tailer exited with error", "err", err)
		os.Exit(1)
	}
	logger.Info("mc-agent stopped cleanly")
}

func sendEventWithRetry(ctx context.Context, client *http.Client, url, secret string, ev events.Event, maxAttempts int) error {
	payload, err := json.Marshal(ev)
	if err != nil {
		return err
	}

	backoff := 500 * time.Millisecond
	var lastErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(webhook.HeaderSecret, secret)

		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
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

		if attempt == maxAttempts {
			break
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
			backoff *= 2
			if backoff > 5*time.Second {
				backoff = 5 * time.Second
			}
		}
	}

	return lastErr
}

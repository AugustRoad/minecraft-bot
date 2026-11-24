package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/AugustRoad/minecraft-bot/internal/config"
	"github.com/AugustRoad/minecraft-bot/internal/controller"
	"github.com/AugustRoad/minecraft-bot/internal/discord"
	"github.com/AugustRoad/minecraft-bot/internal/gce"
	"github.com/AugustRoad/minecraft-bot/internal/minecraft"
	"github.com/AugustRoad/minecraft-bot/internal/webhook"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := config.LoadDotEnv(".env"); err != nil {
		logger.Warn("failed to load .env file", "err", err)
	}

	cfg, err := config.LoadBotConfig()
	if err != nil {
		logger.Error("invalid bot configuration", "err", err)
		os.Exit(1)
	}

	if cfg.RCONPassword == "" {
		logger.Warn("MC_RCON_PASSWORD is empty; RCON graceful stop and Discord->MC chat relay will be disabled")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	gceClient, err := gce.NewClient(ctx, cfg.GCPProject, cfg.GCPZone, cfg.MCInstanceName)
	if err != nil {
		logger.Error("failed to initialize GCE client", "err", err)
		os.Exit(1)
	}
	defer gceClient.Close()

	mcClient := &minecraft.Client{
		MCPort:       cfg.MCPort,
		RCONPort:     cfg.RCONPort,
		RCONPassword: cfg.RCONPassword,
	}

	bot, err := discord.NewBot(cfg, logger)
	if err != nil {
		logger.Error("failed to initialize Discord bot", "err", err)
		os.Exit(1)
	}

	ctrl := controller.New(cfg, gceClient, mcClient, bot, logger)
	bot.SetController(ctrl)

	if err := bot.Start(ctx); err != nil {
		logger.Error("failed to start Discord bot", "err", err)
		os.Exit(1)
	}

	// EnqueueAgentEvent returns immediately so webhook HTTP responses never block on Discord REST.
	webhookServer := webhook.NewServer(cfg.WebhookListenAddr, cfg.WebhookSecret, ctrl.EnqueueAgentEvent, logger)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		ctrl.Run(ctx)
	}()

	go func() {
		defer wg.Done()
		if err := webhookServer.Start(ctx); err != nil {
			logger.Error("webhook server exited with error", "err", err)
			stop()
		}
	}()

	logger.Info("mc-bot running",
		"project", cfg.GCPProject,
		"zone", cfg.GCPZone,
		"instance", cfg.MCInstanceName,
		"webhook_addr", cfg.WebhookListenAddr,
		"idle_timeout", cfg.IdleTimeout.String(),
	)

	<-ctx.Done()
	logger.Info("shutting down mc-bot...")

	// 1. Close Discord gateway and wait for in-flight slash command / chat workers first
	//    while gceClient is still open.
	if err := bot.Close(); err != nil {
		logger.Warn("error closing discord bot", "err", err)
	}
	// 2. Wait for webhook HTTP server and controller loop to finish draining.
	wg.Wait()
	// 3. gceClient.Close() runs via defer after all users of gceClient have exited.
	logger.Info("mc-bot stopped cleanly")
}

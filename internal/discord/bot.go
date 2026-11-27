package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/AugustRoad/minecraft-bot/internal/config"
	"github.com/AugustRoad/minecraft-bot/internal/controller"
)

const (
	colorGreen  = 0x2ECC71
	colorYellow = 0xF1C40F
	colorRed    = 0xE74C3C

	maxDiscordMessageRunes = 1950
	maxEmbedFieldRunes     = 1000
	chatRelayQueueCapacity = 64
)

var (
	dmDisabled    = false
	adminPerm     = int64(discordgo.PermissionAdministrator)
	slashCommands = []*discordgo.ApplicationCommand{
		{
			Name:         "start",
			Description:  "Start the Minecraft GCE VM and announce the new server IP",
			DMPermission: &dmDisabled,
		},
		{
			Name:         "stop",
			Description:  "Gracefully save the Minecraft world and power off the GCE VM",
			DMPermission: &dmDisabled,
		},
		{
			Name:         "status",
			Description:  "Show GCE VM status, current server IP, MOTD, players, and auto-shutdown timer",
			DMPermission: &dmDisabled,
		},
		{
			Name:         "players",
			Description:  "List players currently online on the Minecraft server",
			DMPermission: &dmDisabled,
		},
		{
			Name:                     "cmd",
			Description:              "Run a Minecraft server console command over RCON (Admins only)",
			DMPermission:             &dmDisabled,
			DefaultMemberPermissions: &adminPerm,
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "command",
					Description: "Console command to run (e.g., op Steve, list, gamerule keepInventory true)",
					Required:    true,
				},
			},
		},
	}
)

type chatRelayItem struct {
	author  string
	content string
}

// Bot integrates Discord slash commands, presence updates, announcements, and the chat bridge.
type Bot struct {
	cfg     config.BotConfig
	session *discordgo.Session
	ctrl    *controller.Controller
	logger  *slog.Logger

	rootCtx   context.Context
	readyOnce sync.Once
	readyCh   chan struct{}
	chatQueue chan chatRelayItem
	wg        sync.WaitGroup

	presenceMu      sync.Mutex
	desiredPresence string
	appliedPresence string
}

// NewBot initializes a discordgo session configured with the required intents.
func NewBot(cfg config.BotConfig, logger *slog.Logger) (*Bot, error) {
	if logger == nil {
		logger = slog.Default()
	}

	session, err := discordgo.New("Bot " + cfg.DiscordToken)
	if err != nil {
		return nil, fmt.Errorf("create discord session: %w", err)
	}

	intents := discordgo.IntentsGuilds
	if cfg.ChatChannelID != "" {
		intents |= discordgo.IntentsGuildMessages | discordgo.IntentsMessageContent
	}
	session.Identify.Intents = intents

	b := &Bot{
		cfg:       cfg,
		session:   session,
		logger:    logger,
		rootCtx:   context.Background(),
		readyCh:   make(chan struct{}),
		chatQueue: make(chan chatRelayItem, chatRelayQueueCapacity),
	}

	session.AddHandler(b.handleReady)
	session.AddHandler(b.handleInteraction)
	if cfg.ChatChannelID != "" {
		session.AddHandler(b.handleMessageCreate)
	}

	return b, nil
}

// SetController binds the lifecycle controller before Start is called.
func (b *Bot) SetController(ctrl *controller.Controller) {
	b.ctrl = ctrl
}

// Start opens the Discord gateway connection, waits for READY, starts internal workers,
// and registers slash commands.
func (b *Bot) Start(ctx context.Context) error {
	b.rootCtx = ctx

	if err := b.session.Open(); err != nil {
		return fmt.Errorf("open discord gateway: %w", err)
	}

	// Wait for READY (with fallback to @me REST lookup) so State.User is never nil or racy.
	appID, botUser, err := b.resolveBotIdentity(ctx)
	if err != nil {
		_ = b.session.Close()
		return fmt.Errorf("resolve discord bot identity: %w", err)
	}

	if _, err := b.session.ApplicationCommandBulkOverwrite(appID, b.cfg.DiscordGuildID, slashCommands, discordgo.WithContext(ctx)); err != nil {
		_ = b.session.Close()
		return fmt.Errorf("register slash commands: %w", err)
	}

	if b.cfg.ChatChannelID != "" {
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			b.runChatRelayWorker(ctx)
		}()
	}

	scope := "global"
	if b.cfg.DiscordGuildID != "" {
		scope = "guild:" + b.cfg.DiscordGuildID
	}
	b.logger.Info("discord bot connected and slash commands registered",
		"user", botUser,
		"scope", scope,
	)

	return nil
}

func (b *Bot) resolveBotIdentity(ctx context.Context) (string, string, error) {
	waitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	select {
	case <-b.readyCh:
	case <-waitCtx.Done():
	}

	if b.session.State != nil {
		b.session.State.RLock()
		u := b.session.State.User
		b.session.State.RUnlock()
		if u != nil && u.ID != "" {
			return u.ID, u.Username, nil
		}
	}

	u, err := b.session.User("@me", discordgo.WithContext(ctx))
	if err != nil {
		return "", "", err
	}
	return u.ID, u.Username, nil
}

// Close closes the Discord gateway so no new interactions arrive and waits for in-flight handlers.
func (b *Bot) Close() error {
	err := b.session.Close()
	b.wg.Wait()
	return err
}

// Announce posts a status or IP announcement to the configured announcement channel.
func (b *Bot) Announce(ctx context.Context, message string) error {
	if b.cfg.AnnounceChannelID == "" || strings.TrimSpace(message) == "" {
		return nil
	}
	_, err := b.session.ChannelMessageSendComplex(
		b.cfg.AnnounceChannelID,
		&discordgo.MessageSend{
			Content:         truncateRunes(message, maxDiscordMessageRunes),
			AllowedMentions: &discordgo.MessageAllowedMentions{},
		},
		discordgo.WithContext(ctx),
	)
	if err != nil {
		b.logger.Warn("failed to send discord announcement", "channel", b.cfg.AnnounceChannelID, "err", err)
	}
	return err
}

// SetPresence updates the bot's Discord activity text, serialized to avoid out-of-order updates
// and only caching applied state after UpdateGameStatus succeeds.
func (b *Bot) SetPresence(text string) {
	b.presenceMu.Lock()
	defer b.presenceMu.Unlock()

	b.desiredPresence = text
	if text == b.appliedPresence {
		return
	}
	if !b.isGatewayReady() {
		return
	}
	if err := b.session.UpdateGameStatus(0, text); err != nil {
		b.logger.Debug("failed to update discord presence", "err", err)
		return
	}
	b.appliedPresence = text
}

func (b *Bot) isGatewayReady() bool {
	if b.session.State == nil {
		return false
	}
	b.session.State.RLock()
	ready := b.session.State.User != nil
	b.session.State.RUnlock()
	return ready
}

func (b *Bot) handleReady(s *discordgo.Session, r *discordgo.Ready) {
	b.readyOnce.Do(func() {
		close(b.readyCh)
	})
	b.logger.Info("discord gateway ready", "bot_user", r.User.Username)

	b.presenceMu.Lock()
	defer b.presenceMu.Unlock()
	if b.desiredPresence != "" {
		if err := s.UpdateGameStatus(0, b.desiredPresence); err == nil {
			b.appliedPresence = b.desiredPresence
		}
	}
}

func (b *Bot) handleInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand || b.ctrl == nil {
		return
	}
	if b.rootCtx.Err() != nil {
		return
	}

	name := i.ApplicationCommandData().Name
	requester := interactionUser(i)
	b.logger.Info("slash command invoked", "command", name, "user", requester)

	// Acknowledge immediately so long GCE/RCON operations never hit Discord's 3-second limit.
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	}, discordgo.WithContext(b.rootCtx)); err != nil {
		b.logger.Warn("failed to defer slash command", "command", name, "err", err)
		return
	}

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		switch name {
		case "start":
			b.cmdStart(s, i, requester)
		case "stop":
			b.cmdStop(s, i, requester)
		case "status":
			b.cmdStatus(s, i)
		case "players":
			b.cmdPlayers(s, i)
		case "cmd":
			b.cmdExec(s, i, requester)
		default:
			b.editReply(b.rootCtx, s, i, "Unknown command.")
		}
	}()
}

func (b *Bot) cmdExec(s *discordgo.Session, i *discordgo.InteractionCreate, requester string) {
	ctx, cancel := context.WithTimeout(b.rootCtx, 20*time.Second)
	defer cancel()

	if i.Member == nil || (i.Member.Permissions&discordgo.PermissionAdministrator) == 0 {
		b.editReply(ctx, s, i, "You need **Administrator** permission to run server console commands.")
		return
	}

	var command string
	for _, opt := range i.ApplicationCommandData().Options {
		if opt.Name == "command" {
			command = strings.TrimSpace(opt.StringValue())
			break
		}
	}
	if command == "" {
		b.editReply(ctx, s, i, "Please provide a console command to run.")
		return
	}

	b.logger.Info("executing rcon console command", "user", requester, "command", command)

	out, err := b.ctrl.RunCommand(ctx, command)
	if err != nil {
		b.editReply(ctx, s, i, fmt.Sprintf("Failed to run `%s`: `%v`", command, err))
		return
	}

	if out == "" {
		b.editReply(ctx, s, i, fmt.Sprintf("Executed `%s` *(no output)*.", command))
		return
	}
	safeOut := strings.ReplaceAll(out, "```", "'''")
	b.editReply(ctx, s, i, fmt.Sprintf("Executed `%s`:\n```\n%s\n```", command, truncateRunes(safeOut, 1800)))
}

func (b *Bot) cmdStart(s *discordgo.Session, i *discordgo.InteractionCreate, requester string) {
	bootTimeout := b.cfg.BootTimeout
	if bootTimeout <= 0 {
		bootTimeout = 3 * time.Minute
	}
	ctx, cancel := context.WithTimeout(b.rootCtx, bootTimeout+30*time.Second)
	defer cancel()

	info, err := b.ctrl.StartServer(ctx, requester)
	if err != nil {
		b.editReply(ctx, s, i, fmt.Sprintf("Failed to start Minecraft VM: `%v`", err))
		return
	}

	if info.ExternalIP != "" {
		addr := formatServerAddr(info.ExternalIP, b.cfg.MCPort)
		b.editReply(ctx, s, i, fmt.Sprintf("Minecraft VM is **RUNNING**!\nServer IP: `%s`\n*(If Minecraft is still starting up, a ready alert will appear shortly.)*", addr))
		return
	}
	b.editReply(ctx, s, i, "Minecraft VM is **RUNNING**, waiting for external IP assignment.")
}

func (b *Bot) cmdStop(s *discordgo.Session, i *discordgo.InteractionCreate, requester string) {
	ctx, cancel := context.WithTimeout(b.rootCtx, 3*time.Minute)
	defer cancel()

	err := b.ctrl.StopServer(ctx, "requested by "+requester)
	if err != nil {
		b.editReply(ctx, s, i, fmt.Sprintf("Failed to stop Minecraft VM: `%v`", err))
		return
	}
	b.editReply(ctx, s, i, "Minecraft world saved and GCE VM has been **STOPPED**.")
}

func (b *Bot) cmdStatus(s *discordgo.Session, i *discordgo.InteractionCreate) {
	ctx, cancel := context.WithTimeout(b.rootCtx, 20*time.Second)
	defer cancel()

	snap, err := b.ctrl.GetStatus(ctx)
	if err != nil {
		b.editReply(ctx, s, i, fmt.Sprintf("Failed to fetch server status: `%v`", err))
		return
	}

	embed := buildStatusEmbed(snap, b.cfg.MCInstanceName)
	_, err = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Embeds: &[]*discordgo.MessageEmbed{embed},
	}, discordgo.WithContext(ctx))
	if err != nil {
		b.logger.Warn("failed to edit /status response", "err", err)
	}
}

func (b *Bot) cmdPlayers(s *discordgo.Session, i *discordgo.InteractionCreate) {
	ctx, cancel := context.WithTimeout(b.rootCtx, 15*time.Second)
	defer cancel()

	snap, err := b.ctrl.GetStatus(ctx)
	if err != nil {
		b.editReply(ctx, s, i, fmt.Sprintf("Failed to query server: `%v`", err))
		return
	}

	if snap.Phase == controller.PhaseOffline {
		b.editReply(ctx, s, i, "The Minecraft server is currently **OFFLINE**. Use `/start` to boot it up.")
		return
	}
	if snap.MC == nil {
		b.editReply(ctx, s, i, fmt.Sprintf("The VM is `%s`, but the Minecraft server is still starting up (not responding to pings yet).", snap.VM.Status))
		return
	}
	if snap.MC.OnlinePlayers == 0 {
		b.editReply(ctx, s, i, fmt.Sprintf("**0/%d** players online right now.", snap.MC.MaxPlayers))
		return
	}

	var msg string
	if len(snap.MC.PlayerNames) > 0 {
		msg = fmt.Sprintf("**%d/%d** players online:\n- %s", snap.MC.OnlinePlayers, snap.MC.MaxPlayers, strings.Join(snap.MC.PlayerNames, "\n- "))
	} else {
		msg = fmt.Sprintf("**%d/%d** players online *(server hides player sample list)*.", snap.MC.OnlinePlayers, snap.MC.MaxPlayers)
	}
	b.editReply(ctx, s, i, msg)
}

func (b *Bot) editReply(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	safe := truncateRunes(content, maxDiscordMessageRunes)
	_, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Content:         &safe,
		AllowedMentions: &discordgo.MessageAllowedMentions{},
	}, discordgo.WithContext(ctx))
	if err != nil {
		b.logger.Warn("failed to edit interaction response", "err", err)
	}
}

func buildStatusEmbed(snap controller.Snapshot, instanceName string) *discordgo.MessageEmbed {
	color := colorRed
	title := fmt.Sprintf("Minecraft Server — %s", snap.Phase)
	switch snap.Phase {
	case controller.PhaseOnline:
		if snap.MC != nil {
			color = colorGreen
		} else {
			color = colorYellow
		}
	case controller.PhaseBooting, controller.PhaseStopping:
		color = colorYellow
	}

	fields := []*discordgo.MessageEmbedField{
		{
			Name:   "GCE VM",
			Value:  truncateRunes(fmt.Sprintf("`%s` (%s)", instanceName, snap.VM.Status), maxEmbedFieldRunes),
			Inline: true,
		},
	}

	if snap.VM.ExternalIP != "" {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   "Server IP",
			Value:  fmt.Sprintf("`%s`", formatServerAddr(snap.VM.ExternalIP, snap.MCPort)),
			Inline: true,
		})
	} else {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   "Server IP",
			Value:  "*Assigned on next `/start`*",
			Inline: true,
		})
	}

	if snap.MC != nil {
		playerValue := fmt.Sprintf("**%d / %d**", snap.MC.OnlinePlayers, snap.MC.MaxPlayers)
		if len(snap.MC.PlayerNames) > 0 {
			playerValue += "\n" + strings.Join(snap.MC.PlayerNames, ", ")
		}
		fields = append(fields,
			&discordgo.MessageEmbedField{
				Name:   "Players",
				Value:  truncateRunes(playerValue, maxEmbedFieldRunes),
				Inline: true,
			},
			&discordgo.MessageEmbedField{
				Name:   "Version",
				Value:  truncateRunes(fallbackText(snap.MC.Version, "Unknown"), maxEmbedFieldRunes),
				Inline: true,
			},
			&discordgo.MessageEmbedField{
				Name:   "Internal Ping",
				Value:  fmt.Sprintf("%dms", snap.MC.Latency.Milliseconds()),
				Inline: true,
			},
			&discordgo.MessageEmbedField{
				Name:   "MOTD",
				Value:  truncateRunes(fallbackText(snap.MC.MOTD, "—"), maxEmbedFieldRunes),
				Inline: false,
			},
		)
	} else if snap.Phase == controller.PhaseOnline {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   "Minecraft",
			Value:  "VM is running; waiting for Minecraft server to respond...",
			Inline: false,
		})
	}

	if snap.IdleTrackingActive {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   "Auto-Shutdown",
			Value:  fmt.Sprintf("Empty server — stopping in **%s**", snap.IdleRemaining.Round(time.Second)),
			Inline: false,
		})
	} else if snap.GraceRemaining > 0 {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   "Auto-Shutdown",
			Value:  fmt.Sprintf("Startup grace period (**%s** remaining)", snap.GraceRemaining.Round(time.Second)),
			Inline: false,
		})
	}

	return &discordgo.MessageEmbed{
		Title:     title,
		Color:     color,
		Fields:    fields,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
}

func formatServerAddr(ip string, port int) string {
	if port <= 0 {
		port = 25565
	}
	return fmt.Sprintf("%s:%d", ip, port)
}

func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + "…"
}

func interactionUser(i *discordgo.InteractionCreate) string {
	if i.Member != nil {
		if i.Member.Nick != "" {
			return i.Member.Nick
		}
		if i.Member.User != nil {
			if i.Member.User.GlobalName != "" {
				return i.Member.User.GlobalName
			}
			return i.Member.User.Username
		}
	}
	if i.User != nil {
		if i.User.GlobalName != "" {
			return i.User.GlobalName
		}
		return i.User.Username
	}
	return "unknown"
}

func fallbackText(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

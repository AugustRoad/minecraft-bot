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
)

var slashCommands = []*discordgo.ApplicationCommand{
	{
		Name:        "start",
		Description: "Start the Minecraft GCE VM and announce the new server IP",
	},
	{
		Name:        "stop",
		Description: "Gracefully save the Minecraft world and power off the GCE VM",
	},
	{
		Name:        "status",
		Description: "Show GCE VM status, current server IP, MOTD, players, and auto-shutdown timer",
	},
	{
		Name:        "players",
		Description: "List players currently online on the Minecraft server",
	},
}

// Bot integrates Discord slash commands, presence updates, announcements, and the chat bridge.
type Bot struct {
	cfg     config.BotConfig
	session *discordgo.Session
	ctrl    *controller.Controller
	logger  *slog.Logger

	mu           sync.Mutex
	lastPresence string
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
		cfg:     cfg,
		session: session,
		logger:  logger,
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

// Start opens the Discord gateway connection and registers slash commands.
func (b *Bot) Start(ctx context.Context) error {
	if err := b.session.Open(); err != nil {
		return fmt.Errorf("open discord gateway: %w", err)
	}

	appID := b.session.State.User.ID
	if _, err := b.session.ApplicationCommandBulkOverwrite(appID, b.cfg.DiscordGuildID, slashCommands); err != nil {
		_ = b.session.Close()
		return fmt.Errorf("register slash commands: %w", err)
	}

	scope := "global"
	if b.cfg.DiscordGuildID != "" {
		scope = "guild:" + b.cfg.DiscordGuildID
	}
	b.logger.Info("discord bot connected and slash commands registered",
		"user", b.session.State.User.Username,
		"scope", scope,
	)

	return nil
}

// Close gracefully closes the Discord gateway session.
func (b *Bot) Close() error {
	return b.session.Close()
}

// Announce posts a status or IP announcement to the configured announcement channel.
func (b *Bot) Announce(ctx context.Context, message string) {
	if b.cfg.AnnounceChannelID == "" || strings.TrimSpace(message) == "" {
		return
	}
	_, err := b.session.ChannelMessageSendComplex(b.cfg.AnnounceChannelID, &discordgo.MessageSend{
		Content:         message,
		AllowedMentions: &discordgo.MessageAllowedMentions{},
	})
	if err != nil {
		b.logger.Warn("failed to send discord announcement", "channel", b.cfg.AnnounceChannelID, "err", err)
	}
}

// SetPresence updates the bot's custom Discord activity text (deduplicated to respect rate limits).
func (b *Bot) SetPresence(text string) {
	b.mu.Lock()
	if text == b.lastPresence {
		b.mu.Unlock()
		return
	}
	b.lastPresence = text
	b.mu.Unlock()

	if b.session.State == nil || b.session.State.User == nil {
		return
	}
	if err := b.session.UpdateGameStatus(0, text); err != nil {
		b.logger.Debug("failed to update discord presence", "err", err)
	}
}

func (b *Bot) handleReady(s *discordgo.Session, r *discordgo.Ready) {
	b.logger.Info("discord gateway ready", "bot_user", r.User.Username)
	b.mu.Lock()
	presence := b.lastPresence
	b.mu.Unlock()
	if presence != "" {
		_ = s.UpdateGameStatus(0, presence)
	}
}

func (b *Bot) handleInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand || b.ctrl == nil {
		return
	}

	name := i.ApplicationCommandData().Name
	requester := interactionUser(i)
	b.logger.Info("slash command invoked", "command", name, "user", requester)

	// Acknowledge immediately so long GCE/RCON operations never hit Discord's 3-second limit.
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	}); err != nil {
		b.logger.Warn("failed to defer slash command", "command", name, "err", err)
		return
	}

	go func() {
		switch name {
		case "start":
			b.cmdStart(s, i, requester)
		case "stop":
			b.cmdStop(s, i, requester)
		case "status":
			b.cmdStatus(s, i)
		case "players":
			b.cmdPlayers(s, i)
		default:
			b.editReply(s, i, "Unknown command.")
		}
	}()
}

func (b *Bot) cmdStart(s *discordgo.Session, i *discordgo.InteractionCreate, requester string) {
	ctx, cancel := context.WithTimeout(context.Background(), b.cfg.BootTimeout+30*time.Second)
	defer cancel()

	info, err := b.ctrl.StartServer(ctx, requester)
	if err != nil {
		b.editReply(s, i, fmt.Sprintf("Failed to start Minecraft VM: `%v`", err))
		return
	}

	addr := fmt.Sprintf("%s:%d", info.ExternalIP, b.cfg.MCPort)
	b.editReply(s, i, fmt.Sprintf("Minecraft VM is **RUNNING**!\nServer IP: `%s`\n*(If Minecraft is still starting up, a ready alert will appear shortly.)*", addr))
}

func (b *Bot) cmdStop(s *discordgo.Session, i *discordgo.InteractionCreate, requester string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	err := b.ctrl.StopServer(ctx, "requested by "+requester)
	if err != nil {
		b.editReply(s, i, fmt.Sprintf("Failed to stop Minecraft VM: `%v`", err))
		return
	}
	b.editReply(s, i, "Minecraft world saved and GCE VM has been **STOPPED**.")
}

func (b *Bot) cmdStatus(s *discordgo.Session, i *discordgo.InteractionCreate) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	snap, err := b.ctrl.GetStatus(ctx)
	if err != nil {
		b.editReply(s, i, fmt.Sprintf("Failed to fetch server status: `%v`", err))
		return
	}

	embed := buildStatusEmbed(snap, b.cfg.MCInstanceName)
	_, err = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Embeds: &[]*discordgo.MessageEmbed{embed},
	})
	if err != nil {
		b.logger.Warn("failed to edit /status response", "err", err)
	}
}

func (b *Bot) cmdPlayers(s *discordgo.Session, i *discordgo.InteractionCreate) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	snap, err := b.ctrl.GetStatus(ctx)
	if err != nil {
		b.editReply(s, i, fmt.Sprintf("Failed to query server: `%v`", err))
		return
	}

	if snap.Phase == controller.PhaseOffline {
		b.editReply(s, i, "The Minecraft server is currently **OFFLINE**. Use `/start` to boot it up.")
		return
	}
	if snap.MC == nil {
		b.editReply(s, i, fmt.Sprintf("The VM is `%s`, but the Minecraft server is still starting up (not responding to pings yet).", snap.VM.Status))
		return
	}
	if snap.MC.OnlinePlayers == 0 {
		b.editReply(s, i, fmt.Sprintf("**0/%d** players online right now.", snap.MC.MaxPlayers))
		return
	}

	var msg string
	if len(snap.MC.PlayerNames) > 0 {
		msg = fmt.Sprintf("**%d/%d** players online:\n- %s", snap.MC.OnlinePlayers, snap.MC.MaxPlayers, strings.Join(snap.MC.PlayerNames, "\n- "))
	} else {
		msg = fmt.Sprintf("**%d/%d** players online *(server hides player sample list)*.", snap.MC.OnlinePlayers, snap.MC.MaxPlayers)
	}
	b.editReply(s, i, msg)
}

func (b *Bot) editReply(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	_, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Content:         &content,
		AllowedMentions: &discordgo.MessageAllowedMentions{},
	})
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
			Value:  fmt.Sprintf("`%s` (%s)", instanceName, snap.VM.Status),
			Inline: true,
		},
	}

	if snap.VM.ExternalIP != "" {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   "Server IP",
			Value:  fmt.Sprintf("`%s:%d`", snap.VM.ExternalIP, snap.MCPort),
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
				Value:  playerValue,
				Inline: true,
			},
			&discordgo.MessageEmbedField{
				Name:   "Version",
				Value:  fallbackText(snap.MC.Version, "Unknown"),
				Inline: true,
			},
			&discordgo.MessageEmbedField{
				Name:   "Internal Ping",
				Value:  fmt.Sprintf("%dms", snap.MC.Latency.Milliseconds()),
				Inline: true,
			},
			&discordgo.MessageEmbedField{
				Name:   "MOTD",
				Value:  fallbackText(snap.MC.MOTD, "—"),
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

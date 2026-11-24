package discord

import (
	"context"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/AugustRoad/minecraft-bot/internal/events"
)

// SendGameEvent posts formatted Minecraft game events (chat, join/leave, advancements, deaths)
// into the configured live bridge channel (falling back to the announcement channel).
func (b *Bot) SendGameEvent(ctx context.Context, ev events.Event) {
	channelID := b.cfg.ChatChannelID
	if channelID == "" {
		channelID = b.cfg.AnnounceChannelID
	}
	if channelID == "" {
		return
	}

	var text string
	switch ev.Type {
	case events.TypePlayerJoin:
		text = fmt.Sprintf("➕ **%s** joined the game", escapeDiscord(ev.Player))
	case events.TypePlayerLeave:
		text = fmt.Sprintf("➖ **%s** left the game", escapeDiscord(ev.Player))
	case events.TypeChat:
		text = fmt.Sprintf("💬 **%s**: %s", escapeDiscord(ev.Player), escapeDiscord(ev.Message))
	case events.TypeAdvancement:
		text = fmt.Sprintf("🏆 %s", escapeDiscord(ev.Message))
	case events.TypeDeath:
		text = fmt.Sprintf("💀 %s", escapeDiscord(ev.Message))
	case events.TypeServerStopping:
		text = "🛑 Minecraft server is shutting down..."
	default:
		return
	}

	// Empty AllowedMentions prevents in-game players from triggering @everyone/@here/role pings.
	_, err := b.session.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Content:         text,
		AllowedMentions: &discordgo.MessageAllowedMentions{},
	})
	if err != nil {
		b.logger.Warn("failed to forward game event to discord", "type", ev.Type, "err", err)
	}
}

// handleMessageCreate relays messages written in the live chat bridge channel into Minecraft via RCON.
func (b *Bot) handleMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	if b.ctrl == nil || b.cfg.ChatChannelID == "" {
		return
	}
	if m.ChannelID != b.cfg.ChatChannelID {
		return
	}
	if m.Author == nil || m.Author.Bot || m.WebhookID != "" {
		return
	}

	content := strings.TrimSpace(m.ContentWithMentionsReplaced())
	if content == "" && len(m.Attachments) > 0 {
		content = "[attachment]"
	}
	if content == "" {
		return
	}

	author := m.Author.Username
	if m.Member != nil && m.Member.Nick != "" {
		author = m.Member.Nick
	} else if m.Author.GlobalName != "" {
		author = m.Author.GlobalName
	}

	go func() {
		if err := b.ctrl.RelayDiscordChat(context.Background(), author, content); err != nil {
			b.logger.Debug("could not relay discord chat to minecraft", "author", author, "err", err)
		}
	}()
}

func escapeDiscord(s string) string {
	r := strings.NewReplacer(
		"@everyone", "@\u200beveryone",
		"@here", "@\u200bhere",
	)
	return r.Replace(s)
}

package logtail

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AugustRoad/minecraft-bot/internal/events"
)

func TestParseLine(t *testing.T) {
	now := time.Date(2026, 11, 23, 18, 0, 0, 0, time.UTC)

	tests := []struct {
		name       string
		line       string
		wantOK     bool
		wantType   events.EventType
		wantPlayer string
		wantMsg    string
	}{
		{
			name:     "vanilla server ready",
			line:     `[18:01:02] [Server thread/INFO]: Done (4.512s)! For help, type "help"`,
			wantOK:   true,
			wantType: events.TypeServerReady,
		},
		{
			name:     "forge server ready with logger tag",
			line:     `[18:01:02] [Server thread/INFO] [minecraft/DedicatedServer]: Done (9.811s)! For help, type "help"`,
			wantOK:   true,
			wantType: events.TypeServerReady,
		},
		{
			name:     "server stopping",
			line:     `[18:10:00] [Server thread/INFO]: Stopping the server`,
			wantOK:   true,
			wantType: events.TypeServerStopping,
		},
		{
			name:       "player join",
			line:       `[18:02:11] [Server thread/INFO]: Steve joined the game`,
			wantOK:     true,
			wantType:   events.TypePlayerJoin,
			wantPlayer: "Steve",
		},
		{
			name:       "player leave",
			line:       `[18:05:44] [Server thread/INFO]: Alex_99 left the game`,
			wantOK:     true,
			wantType:   events.TypePlayerLeave,
			wantPlayer: "Alex_99",
		},
		{
			name:       "paper async chat",
			line:       `[18:03:00] [Async Chat Thread - #0/INFO]: <Steve> anyone got iron?`,
			wantOK:     true,
			wantType:   events.TypeChat,
			wantPlayer: "Steve",
			wantMsg:    "anyone got iron?",
		},
		{
			name:       "unsigned 1.19+ chat",
			line:       `[18:03:01] [Server thread/INFO]: [Not Secure] <Alex_99> check the chest by spawn`,
			wantOK:     true,
			wantType:   events.TypeChat,
			wantPlayer: "Alex_99",
			wantMsg:    "check the chest by spawn",
		},
		{
			name:       "advancement",
			line:       `[18:04:00] [Server thread/INFO]: Steve has made the advancement [Diamonds!]`,
			wantOK:     true,
			wantType:   events.TypeAdvancement,
			wantPlayer: "Steve",
			wantMsg:    "Steve has made the advancement [Diamonds!]",
		},
		{
			name:       "death message",
			line:       `[18:04:22] [Server thread/INFO]: Steve tried to swim in lava`,
			wantOK:     true,
			wantType:   events.TypeDeath,
			wantPlayer: "Steve",
			wantMsg:    "Steve tried to swim in lava",
		},
		{
			name:   "unrelated startup noise ignored",
			line:   `[18:00:10] [Server thread/INFO]: Preparing spawn area: 64%`,
			wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev, ok := ParseLine(tc.line, now)
			if ok != tc.wantOK {
				t.Fatalf("ParseLine ok=%v, want %v (ev=%+v)", ok, tc.wantOK, ev)
			}
			if !ok {
				return
			}
			if ev.Type != tc.wantType {
				t.Errorf("Type=%q, want %q", ev.Type, tc.wantType)
			}
			if tc.wantPlayer != "" && ev.Player != tc.wantPlayer {
				t.Errorf("Player=%q, want %q", ev.Player, tc.wantPlayer)
			}
			if tc.wantMsg != "" && ev.Message != tc.wantMsg {
				t.Errorf("Message=%q, want %q", ev.Message, tc.wantMsg)
			}
		})
	}
}

func TestTailFile_HandlesRotation(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "latest.log")

	// Pre-populate old content that should NOT be replayed
	if err := os.WriteFile(logPath, []byte("[10:00:00] [Server thread/INFO]: OldLine\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lines := make(chan string, 10)
	go func() {
		_ = TailFile(ctx, logPath, 20*time.Millisecond, nil, func(l string) {
			lines <- l
		})
	}()

	time.Sleep(50 * time.Millisecond)

	// Append a live line
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("[10:01:00] [Server thread/INFO]: Steve joined the game\n")
	_ = f.Close()

	select {
	case got := <-lines:
		if got != "[10:01:00] [Server thread/INFO]: Steve joined the game" {
			t.Fatalf("unexpected first line: %q", got)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for appended line")
	}

	// Simulate log rotation: rename old latest.log and create a brand new latest.log
	if err := os.Rename(logPath, filepath.Join(dir, "2026-11-23-1.log")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("[10:02:00] [Server thread/INFO]: Done (2.100s)! For help, type \"help\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-lines:
		if got != `[10:02:00] [Server thread/INFO]: Done (2.100s)! For help, type "help"` {
			t.Fatalf("unexpected rotated line: %q", got)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for rotated line")
	}
}

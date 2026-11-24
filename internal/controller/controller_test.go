package controller

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AugustRoad/minecraft-bot/internal/config"
	"github.com/AugustRoad/minecraft-bot/internal/events"
	"github.com/AugustRoad/minecraft-bot/internal/gce"
	"github.com/AugustRoad/minecraft-bot/internal/minecraft"
)

type fakeVM struct {
	mu          sync.Mutex
	info        gce.InstanceInfo
	nextExtIPs  []string
	startCalls  int
	stopCalls   int
	ipCallIndex int
}

func (f *fakeVM) GetStatus(ctx context.Context) (gce.InstanceInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.info, nil
}

func (f *fakeVM) Start(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCalls++
	ip := "34.1.2.3"
	if f.ipCallIndex < len(f.nextExtIPs) {
		ip = f.nextExtIPs[f.ipCallIndex]
		f.ipCallIndex++
	}
	f.info = gce.InstanceInfo{
		Name:       "mc-vm",
		Status:     gce.StatusRunning,
		ExternalIP: ip,
		InternalIP: "10.128.0.5",
	}
	return nil
}

func (f *fakeVM) Stop(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopCalls++
	f.info = gce.InstanceInfo{
		Name:   "mc-vm",
		Status: gce.StatusTerminated,
	}
	return nil
}

func (f *fakeVM) Close() error { return nil }

type fakeMC struct {
	mu           sync.Mutex
	online       bool
	players      int
	stopCalled   bool
	chatMessages []string
}

func (f *fakeMC) QueryStatus(ctx context.Context, host string) (minecraft.ServerStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.online {
		return minecraft.ServerStatus{}, errors.New("connection refused")
	}
	return minecraft.ServerStatus{
		Version:       "1.21.4",
		MOTD:          "Test Server",
		OnlinePlayers: f.players,
		MaxPlayers:    20,
	}, nil
}

func (f *fakeMC) SendDiscordChat(ctx context.Context, host, author, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chatMessages = append(f.chatMessages, author+": "+message)
	return nil
}

func (f *fakeMC) BroadcastNotice(ctx context.Context, host, notice string) error { return nil }

func (f *fakeMC) GracefulStop(ctx context.Context, host string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopCalled = true
	f.online = false
	return nil
}

type fakeNotifier struct {
	mu            sync.Mutex
	announcements []string
	gameEvents    []events.Event
	lastPresence  string
}

func (n *fakeNotifier) Announce(ctx context.Context, msg string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.announcements = append(n.announcements, msg)
}

func (n *fakeNotifier) SendGameEvent(ctx context.Context, ev events.Event) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.gameEvents = append(n.gameEvents, ev)
}

func (n *fakeNotifier) SetPresence(text string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.lastPresence = text
}

func (n *fakeNotifier) countContaining(substr string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	count := 0
	for _, a := range n.announcements {
		if strings.Contains(a, substr) {
			count++
		}
	}
	return count
}

func TestStartAndStopAnnouncesNewIPEveryTime(t *testing.T) {
	vm := &fakeVM{
		info:       gce.InstanceInfo{Name: "mc-vm", Status: gce.StatusTerminated},
		nextExtIPs: []string{"34.10.10.1", "34.20.20.2"},
	}
	mc := &fakeMC{online: true, players: 0}
	notifier := &fakeNotifier{}

	cfg := config.BotConfig{
		MCInstanceName:     "mc-vm",
		MCPort:             25565,
		RCONPassword:       "secret",
		StartupGracePeriod: 10 * time.Minute,
		IdleTimeout:        15 * time.Minute,
		IdleWarningBefore:  2 * time.Minute,
		BootPollInterval:   10 * time.Millisecond,
		BootTimeout:        2 * time.Second,
		RCONStopTimeout:    2 * time.Second,
	}

	ctrl := New(cfg, vm, mc, notifier, nil)
	ctx := context.Background()

	// Boot #1 -> announces first ephemeral IP
	info1, err := ctrl.StartServer(ctx, "Alice")
	if err != nil {
		t.Fatalf("first StartServer failed: %v", err)
	}
	if info1.ExternalIP != "34.10.10.1" {
		t.Fatalf("got IP %s, want 34.10.10.1", info1.ExternalIP)
	}
	if notifier.countContaining("34.10.10.1:25565") == 0 {
		t.Fatalf("expected announcement with 34.10.10.1:25565, got: %v", notifier.announcements)
	}

	// Stop -> uses RCON graceful stop and stops GCE VM
	if err := ctrl.StopServer(ctx, "manual test"); err != nil {
		t.Fatalf("StopServer failed: %v", err)
	}
	if !mc.stopCalled || vm.stopCalls != 1 {
		t.Fatalf("expected graceful RCON stop and VM stop; rcon=%v vmStops=%d", mc.stopCalled, vm.stopCalls)
	}

	// Boot #2 -> announces second ephemeral IP
	mc.online = true
	info2, err := ctrl.StartServer(ctx, "Bob")
	if err != nil {
		t.Fatalf("second StartServer failed: %v", err)
	}
	if info2.ExternalIP != "34.20.20.2" {
		t.Fatalf("got IP %s, want 34.20.20.2", info2.ExternalIP)
	}
	if notifier.countContaining("34.20.20.2:25565") == 0 {
		t.Fatalf("expected announcement with 34.20.20.2:25565, got: %v", notifier.announcements)
	}
}

func TestIdleAutoShutdownWithGraceAndWarning(t *testing.T) {
	vm := &fakeVM{
		info: gce.InstanceInfo{
			Name:       "mc-vm",
			Status:     gce.StatusRunning,
			ExternalIP: "35.5.5.5",
			InternalIP: "10.128.0.9",
		},
	}
	mc := &fakeMC{online: true, players: 0}
	notifier := &fakeNotifier{}

	cfg := config.BotConfig{
		MCInstanceName:     "mc-vm",
		MCPort:             25565,
		RCONPassword:       "secret",
		StartupGracePeriod: 10 * time.Minute,
		IdleTimeout:        15 * time.Minute,
		IdleWarningBefore:  2 * time.Minute,
		RCONStopTimeout:    1 * time.Second,
	}

	ctrl := New(cfg, vm, mc, notifier, nil)
	clock := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	ctrl.now = func() time.Time { return clock }

	ctx := context.Background()

	// T+0m: Initial poll detects RUNNING VM, announces IP, enters 10m startup grace period.
	ctrl.PollOnce(ctx)
	if notifier.countContaining("35.5.5.5:25565") == 0 {
		t.Fatalf("expected external boot IP announcement, got %v", notifier.announcements)
	}

	// T+9m: Still in grace period; must not start idle countdown or stop VM.
	clock = clock.Add(9 * time.Minute)
	ctrl.PollOnce(ctx)
	if vm.stopCalls != 0 {
		t.Fatalf("should not stop during startup grace period")
	}

	// T+11m: Grace period expired -> idle timer starts at T+11m.
	clock = clock.Add(2 * time.Minute)
	ctrl.PollOnce(ctx)
	if vm.stopCalls != 0 || notifier.countContaining("auto-shutdown in") != 0 {
		t.Fatalf("unexpected early warning/stop right after grace period")
	}

	// T+24m30s (13m30s idle -> 1m30s left): should post idle warning.
	clock = clock.Add(13*time.Minute + 30*time.Second)
	ctrl.PollOnce(ctx)
	if notifier.countContaining("auto-shutdown in") != 1 {
		t.Fatalf("expected 1 idle warning, got %v", notifier.announcements)
	}
	if vm.stopCalls != 0 {
		t.Fatalf("should not stop yet before full idle timeout")
	}

	// T+26m30s (15m30s idle): should gracefully stop Minecraft and power off VM.
	clock = clock.Add(2 * time.Minute)
	ctrl.PollOnce(ctx)
	if vm.stopCalls != 1 || !mc.stopCalled {
		t.Fatalf("expected auto-shutdown to trigger; vmStops=%d rconStop=%v", vm.stopCalls, mc.stopCalled)
	}
}

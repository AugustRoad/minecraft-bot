package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/AugustRoad/minecraft-bot/internal/config"
	"github.com/AugustRoad/minecraft-bot/internal/events"
	"github.com/AugustRoad/minecraft-bot/internal/gce"
	"github.com/AugustRoad/minecraft-bot/internal/minecraft"
)

// Phase describes the high-level lifecycle state of the Minecraft VM and server.
type Phase string

const (
	PhaseOffline  Phase = "OFFLINE"
	PhaseBooting  Phase = "BOOTING"
	PhaseOnline   Phase = "ONLINE"
	PhaseStopping Phase = "STOPPING"
)

// Notifier delivers user-facing updates to Discord (or a mock in tests).
type Notifier interface {
	Announce(ctx context.Context, message string)
	SendGameEvent(ctx context.Context, ev events.Event)
	SetPresence(text string)
}

// Snapshot is a point-in-time read-only view of VM + Minecraft status for /status and /players.
type Snapshot struct {
	Phase              Phase
	VM                 gce.InstanceInfo
	MC                 *minecraft.ServerStatus
	MCReady            bool
	MCPort             int
	GraceRemaining     time.Duration
	IdleRemaining      time.Duration
	IdleTrackingActive bool
}

// Controller coordinates GCE VM lifecycle, ephemeral IP announcements, SLP/RCON,
// mc-agent events, and idle auto-shutdown.
type Controller struct {
	cfg      config.BotConfig
	vm       gce.VMManager
	mc       minecraft.ServerClient
	notifier Notifier
	logger   *slog.Logger
	now      func() time.Time

	mu                    sync.Mutex
	phase                 Phase
	vmInfo                gce.InstanceInfo
	mcStatus              *minecraft.ServerStatus
	mcReady               bool
	lastAnnouncedIP       string
	readyAnnouncedForIP   string
	lastBootAt            time.Time
	hadPlayersThisSession bool
	emptySince            time.Time
	idleWarned            bool
	opInProgress          bool
}

// New creates a new Controller.
func New(cfg config.BotConfig, vm gce.VMManager, mc minecraft.ServerClient, notifier Notifier, logger *slog.Logger) *Controller {
	if logger == nil {
		logger = slog.Default()
	}
	return &Controller{
		cfg:      cfg,
		vm:       vm,
		mc:       mc,
		notifier: notifier,
		logger:   logger,
		now:      time.Now,
		phase:    PhaseOffline,
	}
}

// Run starts the background reconciliation and idle auto-shutdown loop until ctx is canceled.
func (c *Controller) Run(ctx context.Context) {
	// Initial sync on bot startup
	c.PollOnce(ctx)

	interval := c.cfg.PollInterval
	if interval <= 0 {
		interval = 60 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.PollOnce(ctx)
		}
	}
}

// StartServer boots the Minecraft GCE VM, polls until GCE assigns a fresh ephemeral IP,
// and announces the IP in Discord. It returns the resulting InstanceInfo once running.
func (c *Controller) StartServer(ctx context.Context, requestedBy string) (gce.InstanceInfo, error) {
	c.mu.Lock()
	if c.opInProgress {
		c.mu.Unlock()
		return gce.InstanceInfo{}, errors.New("a start or stop operation is already in progress")
	}
	c.opInProgress = true
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.opInProgress = false
		c.mu.Unlock()
	}()

	info, err := c.vm.GetStatus(ctx)
	if err != nil {
		return gce.InstanceInfo{}, fmt.Errorf("check VM status: %w", err)
	}

	if info.IsRunning() {
		c.handleRunningVM(ctx, info, false)
		return info, nil
	}

	c.mu.Lock()
	c.phase = PhaseBooting
	c.vmInfo = info
	c.mcReady = false
	c.mcStatus = nil
	c.lastAnnouncedIP = ""
	c.readyAnnouncedForIP = ""
	c.mu.Unlock()

	c.setPresence("Booting Minecraft VM...")
	c.announce(ctx, fmt.Sprintf("Starting Minecraft VM (`%s`) — requested by **%s**. Waiting for GCE to assign a new IP...", c.cfg.MCInstanceName, requestedBy))

	if err := c.vm.Start(ctx); err != nil {
		c.mu.Lock()
		c.phase = PhaseOffline
		c.mu.Unlock()
		c.setPresence("Offline — /start to play")
		return gce.InstanceInfo{}, fmt.Errorf("start VM: %w", err)
	}

	// Poll until GCE reports RUNNING with an assigned ephemeral external IP
	bootTimeout := c.cfg.BootTimeout
	if bootTimeout <= 0 {
		bootTimeout = 3 * time.Minute
	}
	pollEvery := c.cfg.BootPollInterval
	if pollEvery <= 0 {
		pollEvery = 3 * time.Second
	}

	pollCtx, cancel := context.WithTimeout(ctx, bootTimeout)
	defer cancel()

	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()

	for {
		info, err = c.vm.GetStatus(pollCtx)
		if err == nil && info.IsRunning() && info.ExternalIP != "" {
			c.handleRunningVM(ctx, info, true)
			return info, nil
		}

		select {
		case <-pollCtx.Done():
			return info, fmt.Errorf("timed out waiting for VM to reach RUNNING with an external IP: %w", pollCtx.Err())
		case <-ticker.C:
		}
	}
}

// StopServer gracefully flushes & stops Minecraft over RCON, then powers off the GCE VM.
func (c *Controller) StopServer(ctx context.Context, reason string) error {
	c.mu.Lock()
	if c.opInProgress {
		c.mu.Unlock()
		return errors.New("a start or stop operation is already in progress")
	}
	c.opInProgress = true
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.opInProgress = false
		c.mu.Unlock()
	}()

	info, err := c.vm.GetStatus(ctx)
	if err != nil {
		return fmt.Errorf("check VM status: %w", err)
	}
	if info.IsStopped() {
		c.resetOfflineState(info)
		return nil
	}

	c.mu.Lock()
	c.phase = PhaseStopping
	c.vmInfo = info
	c.mu.Unlock()

	c.setPresence("Shutting down server...")
	c.announce(ctx, fmt.Sprintf("Stopping Minecraft server (%s). Saving world before powering off VM...", reason))

	// Step 1: Try graceful RCON save-all + stop over internal VPC IP
	if info.InternalIP != "" && c.cfg.RCONPassword != "" {
		rconCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := c.mc.GracefulStop(rconCtx, info.InternalIP)
		cancel()
		if err != nil {
			c.logger.Warn("rcon graceful stop failed, proceeding to VM stop", "err", err)
		} else {
			c.waitForMinecraftDown(ctx, info.InternalIP)
		}
	}

	// Step 2: Stop the GCE VM instance
	if err := c.vm.Stop(ctx); err != nil {
		c.mu.Lock()
		c.phase = PhaseOnline
		c.mu.Unlock()
		return fmt.Errorf("stop VM: %w", err)
	}

	stoppedInfo := gce.InstanceInfo{
		Name:   info.Name,
		Status: gce.StatusTerminated,
	}
	c.resetOfflineState(stoppedInfo)
	c.announce(ctx, fmt.Sprintf("Minecraft VM (`%s`) is now **OFFLINE** (%s). Use `/start` anytime to spin it back up.", c.cfg.MCInstanceName, reason))
	return nil
}

// GetStatus refreshes live GCE and Minecraft SLP status and returns a Snapshot.
func (c *Controller) GetStatus(ctx context.Context) (Snapshot, error) {
	info, err := c.vm.GetStatus(ctx)
	if err != nil {
		return Snapshot{}, err
	}

	if info.IsStopped() {
		c.resetOfflineState(info)
		return c.snapshot(), nil
	}

	if info.IsRunning() {
		c.handleRunningVM(ctx, info, false)
		c.refreshMinecraftStatus(ctx, info)
	} else {
		c.mu.Lock()
		c.vmInfo = info
		if info.Status == gce.StatusStopping {
			c.phase = PhaseStopping
		} else {
			c.phase = PhaseBooting
		}
		c.mu.Unlock()
	}

	return c.snapshot(), nil
}

// RelayDiscordChat forwards a Discord user message into Minecraft via RCON tellraw.
func (c *Controller) RelayDiscordChat(ctx context.Context, author, message string) error {
	c.mu.Lock()
	host := c.vmInfo.InternalIP
	running := c.vmInfo.IsRunning()
	c.mu.Unlock()

	if !running || host == "" {
		return nil
	}
	rconCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return c.mc.SendDiscordChat(rconCtx, host, author, message)
}

// HandleAgentEvent processes structured events pushed from mc-agent over the internal VPC webhook.
func (c *Controller) HandleAgentEvent(ctx context.Context, ev events.Event) {
	c.logger.Info("received mc-agent event", "type", ev.Type, "player", ev.Player)

	switch ev.Type {
	case events.TypeVMBoot:
		// External or scheduled VM boot: immediately query GCE so the new IP is announced right away.
		go c.syncBootedVM(context.Background())

	case events.TypeServerReady:
		go c.handleServerReady(context.Background())

	case events.TypePlayerJoin:
		c.mu.Lock()
		c.mcReady = true
		c.hadPlayersThisSession = true
		c.emptySince = time.Time{}
		c.idleWarned = false
		c.mu.Unlock()

		c.sendGameEvent(ctx, ev)
		go c.refreshCurrentMC(context.Background())

	case events.TypePlayerLeave:
		c.sendGameEvent(ctx, ev)
		go c.refreshCurrentMC(context.Background())

	case events.TypeChat, events.TypeAdvancement, events.TypeDeath, events.TypeServerStopping:
		c.sendGameEvent(ctx, ev)
	}
}

// PollOnce performs one reconciliation tick: checks GCE state, queries Minecraft SLP,
// updates presence, detects external boots/IP changes, and enforces idle auto-shutdown.
func (c *Controller) PollOnce(ctx context.Context) {
	c.mu.Lock()
	busy := c.opInProgress
	c.mu.Unlock()
	if busy {
		return
	}

	pollCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	info, err := c.vm.GetStatus(pollCtx)
	if err != nil {
		c.logger.Warn("poll gce status failed", "err", err)
		return
	}

	if info.IsStopped() {
		c.resetOfflineState(info)
		return
	}

	if !info.IsRunning() {
		c.mu.Lock()
		c.vmInfo = info
		if info.Status == gce.StatusStopping {
			c.phase = PhaseStopping
			c.mu.Unlock()
			c.setPresence("Shutting down server...")
		} else {
			c.phase = PhaseBooting
			c.mu.Unlock()
			c.setPresence("Booting Minecraft VM...")
		}
		return
	}

	// VM is RUNNING
	c.handleRunningVM(pollCtx, info, false)
	status, ok := c.refreshMinecraftStatus(pollCtx, info)
	if !ok {
		return
	}

	// Evaluate idle auto-shutdown when Minecraft is responsive
	if c.cfg.IdleTimeout <= 0 {
		return
	}

	now := c.now()
	var shouldWarn bool
	var warnRemaining time.Duration
	var shouldStop bool

	c.mu.Lock()
	if status.OnlinePlayers > 0 {
		c.hadPlayersThisSession = true
		c.emptySince = time.Time{}
		c.idleWarned = false
	} else {
		inGrace := !c.hadPlayersThisSession && now.Sub(c.lastBootAt) < c.cfg.StartupGracePeriod
		if !inGrace {
			if c.emptySince.IsZero() {
				c.emptySince = now
			}
			elapsed := now.Sub(c.emptySince)
			remaining := c.cfg.IdleTimeout - elapsed
			if remaining <= 0 {
				shouldStop = true
			} else if !c.idleWarned && c.cfg.IdleWarningBefore > 0 && remaining <= c.cfg.IdleWarningBefore {
				c.idleWarned = true
				shouldWarn = true
				warnRemaining = remaining.Round(time.Second)
			}
		}
	}
	c.mu.Unlock()

	if shouldWarn {
		c.announce(ctx, fmt.Sprintf("No players online — auto-shutdown in **%s** unless someone joins.", formatDuration(warnRemaining)))
	}
	if shouldStop {
		stopCtx, stopCancel := context.WithTimeout(ctx, 2*time.Minute)
		defer stopCancel()
		reason := fmt.Sprintf("auto-shutdown after %s idle with 0 players", formatDuration(c.cfg.IdleTimeout))
		if err := c.StopServer(stopCtx, reason); err != nil {
			c.logger.Error("auto-shutdown failed", "err", err)
			c.announce(ctx, fmt.Sprintf("Auto-shutdown attempted but encountered an error: `%v`", err))
		}
	}
}

func (c *Controller) syncBootedVM(ctx context.Context) {
	syncCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	info, err := c.vm.GetStatus(syncCtx)
	if err != nil {
		c.logger.Warn("syncBootedVM failed", "err", err)
		return
	}
	if info.IsRunning() {
		c.handleRunningVM(syncCtx, info, false)
	}
}

func (c *Controller) handleServerReady(ctx context.Context) {
	syncCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	info, err := c.vm.GetStatus(syncCtx)
	if err == nil && info.IsRunning() {
		c.handleRunningVM(syncCtx, info, false)
	} else {
		c.mu.Lock()
		info = c.vmInfo
		c.mu.Unlock()
	}

	var shouldAnnounceReady bool
	var addr string

	c.mu.Lock()
	c.mcReady = true
	if c.lastBootAt.IsZero() {
		c.lastBootAt = c.now()
	}
	if info.ExternalIP != "" && c.readyAnnouncedForIP != info.ExternalIP {
		c.readyAnnouncedForIP = info.ExternalIP
		shouldAnnounceReady = true
		addr = c.formatAddress(info.ExternalIP)
	}
	c.mu.Unlock()

	if shouldAnnounceReady {
		c.announce(ctx, fmt.Sprintf("Minecraft server is **READY**! Connect at `%s`", addr))
	}
	c.refreshMinecraftStatus(syncCtx, info)
}

func (c *Controller) handleRunningVM(ctx context.Context, info gce.InstanceInfo, fromStartCmd bool) {
	var announceIP string

	c.mu.Lock()
	prevPhase := c.phase
	c.vmInfo = info
	c.phase = PhaseOnline
	if prevPhase != PhaseOnline || c.lastBootAt.IsZero() {
		c.lastBootAt = c.now()
		c.hadPlayersThisSession = false
		c.emptySince = time.Time{}
		c.idleWarned = false
	}
	// Guarantee IP announcement on every single boot or whenever GCE assigns a new external IP.
	if info.ExternalIP != "" && info.ExternalIP != c.lastAnnouncedIP {
		c.lastAnnouncedIP = info.ExternalIP
		announceIP = info.ExternalIP
	}
	mcReady := c.mcReady
	c.mu.Unlock()

	if announceIP != "" {
		addr := c.formatAddress(announceIP)
		if fromStartCmd {
			c.announce(ctx, fmt.Sprintf("Minecraft VM is **ONLINE**!\nServer IP: `%s`\n*(Waiting for Minecraft server to finish loading...)*", addr))
		} else {
			c.announce(ctx, fmt.Sprintf("Minecraft VM is **ONLINE** with a new IP!\nServer IP: `%s`", addr))
		}
	}

	if !mcReady {
		c.setPresence("Minecraft starting...")
	}
}

func (c *Controller) refreshCurrentMC(ctx context.Context) {
	c.mu.Lock()
	info := c.vmInfo
	c.mu.Unlock()
	if !info.IsRunning() {
		return
	}
	qCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	c.refreshMinecraftStatus(qCtx, info)
}

func (c *Controller) refreshMinecraftStatus(ctx context.Context, info gce.InstanceInfo) (minecraft.ServerStatus, bool) {
	host := info.InternalIP
	if host == "" {
		host = info.ExternalIP
	}
	if host == "" {
		return minecraft.ServerStatus{}, false
	}

	status, err := c.mc.QueryStatus(ctx, host)
	if err != nil {
		c.mu.Lock()
		c.mcStatus = nil
		c.mu.Unlock()
		c.setPresence("Minecraft loading...")
		return minecraft.ServerStatus{}, false
	}

	var announceReadyAddr string

	c.mu.Lock()
	stCopy := status
	c.mcStatus = &stCopy
	c.mcReady = true
	if info.ExternalIP != "" && c.readyAnnouncedForIP != info.ExternalIP {
		c.readyAnnouncedForIP = info.ExternalIP
		announceReadyAddr = c.formatAddress(info.ExternalIP)
	}
	c.mu.Unlock()

	if announceReadyAddr != "" {
		c.announce(ctx, fmt.Sprintf("Minecraft server is **READY**! Connect at `%s`", announceReadyAddr))
	}

	c.setPresence(fmt.Sprintf("Minecraft (%d/%d online)", status.OnlinePlayers, status.MaxPlayers))
	return status, true
}

func (c *Controller) waitForMinecraftDown(ctx context.Context, host string) {
	timeout := c.cfg.RCONStopTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		qCtx, qCancel := context.WithTimeout(waitCtx, 2*time.Second)
		_, err := c.mc.QueryStatus(qCtx, host)
		qCancel()
		if err != nil {
			// Port closed — Minecraft exited cleanly
			return
		}
		select {
		case <-waitCtx.Done():
			c.logger.Warn("timed out waiting for minecraft server to exit after rcon stop")
			return
		case <-ticker.C:
		}
	}
}

func (c *Controller) resetOfflineState(info gce.InstanceInfo) {
	c.mu.Lock()
	c.phase = PhaseOffline
	c.vmInfo = info
	c.mcStatus = nil
	c.mcReady = false
	c.lastAnnouncedIP = ""
	c.readyAnnouncedForIP = ""
	c.lastBootAt = time.Time{}
	c.hadPlayersThisSession = false
	c.emptySince = time.Time{}
	c.idleWarned = false
	c.mu.Unlock()

	c.setPresence("Offline — /start to play")
}

func (c *Controller) snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()

	snap := Snapshot{
		Phase:   c.phase,
		VM:      c.vmInfo,
		MCReady: c.mcReady,
		MCPort:  c.cfg.MCPort,
	}
	if c.mcStatus != nil {
		stCopy := *c.mcStatus
		snap.MC = &stCopy
	}

	now := c.now()
	if c.phase == PhaseOnline && c.cfg.IdleTimeout > 0 {
		if !c.emptySince.IsZero() {
			rem := c.cfg.IdleTimeout - now.Sub(c.emptySince)
			if rem < 0 {
				rem = 0
			}
			snap.IdleTrackingActive = true
			snap.IdleRemaining = rem
		} else if !c.hadPlayersThisSession && !c.lastBootAt.IsZero() {
			rem := c.cfg.StartupGracePeriod - now.Sub(c.lastBootAt)
			if rem > 0 {
				snap.GraceRemaining = rem
			}
		}
	}

	return snap
}

func (c *Controller) formatAddress(ip string) string {
	if c.cfg.MCPort > 0 && c.cfg.MCPort != 25565 {
		return fmt.Sprintf("%s:%d", ip, c.cfg.MCPort)
	}
	return fmt.Sprintf("%s:%d", ip, 25565)
}

func (c *Controller) announce(ctx context.Context, msg string) {
	if c.notifier != nil {
		c.notifier.Announce(ctx, msg)
	}
}

func (c *Controller) sendGameEvent(ctx context.Context, ev events.Event) {
	if c.notifier != nil {
		c.notifier.SendGameEvent(ctx, ev)
	}
}

func (c *Controller) setPresence(text string) {
	if c.notifier != nil {
		c.notifier.SetPresence(text)
	}
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d >= time.Minute {
		mins := int(d / time.Minute)
		secs := int((d % time.Minute) / time.Second)
		if secs == 0 {
			return fmt.Sprintf("%dm", mins)
		}
		return fmt.Sprintf("%dm%ds", mins, secs)
	}
	return d.String()
}

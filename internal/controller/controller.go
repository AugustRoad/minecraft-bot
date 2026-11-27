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

	agentEventQueueSize    = 128
	autoStopRetryCooldown  = 5 * time.Minute
	defaultPostCloseSettle = 5 * time.Second
)

// Notifier delivers user-facing updates to Discord (or a mock in tests).
type Notifier interface {
	Announce(ctx context.Context, message string) error
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

	agentEvents chan events.Event

	mu                    sync.Mutex
	stateEpoch            uint64
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
	autoStopCooldownUntil time.Time
	opInProgress          bool
}

// New creates a new Controller.
func New(cfg config.BotConfig, vm gce.VMManager, mc minecraft.ServerClient, notifier Notifier, logger *slog.Logger) *Controller {
	if logger == nil {
		logger = slog.Default()
	}
	return &Controller{
		cfg:         cfg,
		vm:          vm,
		mc:          mc,
		notifier:    notifier,
		logger:      logger,
		now:         time.Now,
		agentEvents: make(chan events.Event, agentEventQueueSize),
		phase:       PhaseOffline,
	}
}

// Run starts the background reconciliation loop and the mc-agent event worker until ctx is canceled.
func (c *Controller) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.runAgentEventWorker(ctx)
	}()

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
			wg.Wait()
			return
		case <-ticker.C:
			c.PollOnce(ctx)
		}
	}
}

func (c *Controller) runAgentEventWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-c.agentEvents:
			c.HandleAgentEvent(ctx, ev)
		}
	}
}

// EnqueueAgentEvent queues an incoming webhook event without blocking the HTTP response.
func (c *Controller) EnqueueAgentEvent(_ context.Context, ev events.Event) {
	select {
	case c.agentEvents <- ev:
	default:
		c.logger.Warn("agent event queue full; dropping event", "type", ev.Type)
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
	c.stateEpoch++
	opEpoch := c.stateEpoch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.opInProgress = false
		c.stateEpoch++
		c.mu.Unlock()
	}()

	info, err := c.vm.GetStatus(ctx)
	if err != nil {
		return gce.InstanceInfo{}, fmt.Errorf("check VM status: %w", err)
	}

	if info.IsRunning() {
		c.handleRunningVM(ctx, info, false, opEpoch, true)
		return info, nil
	}

	c.mu.Lock()
	c.phase = PhaseBooting
	c.vmInfo = info
	c.mcReady = false
	c.mcStatus = nil
	c.lastAnnouncedIP = ""
	c.readyAnnouncedForIP = ""
	c.autoStopCooldownUntil = time.Time{}
	c.mu.Unlock()

	c.setPresence("Booting Minecraft VM...")
	_ = c.announce(ctx, fmt.Sprintf("Starting Minecraft VM (`%s`) — requested by **%s**. Waiting for GCE to assign a new IP...", c.cfg.MCInstanceName, requestedBy))

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
			c.handleRunningVM(ctx, info, true, opEpoch, true)
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
	c.stateEpoch++
	c.mu.Unlock()

	return c.executeStop(ctx, reason)
}

// executeStop performs the full stop sequence assuming c.opInProgress is already true.
func (c *Controller) executeStop(ctx context.Context, reason string) error {
	defer func() {
		c.mu.Lock()
		c.opInProgress = false
		c.stateEpoch++
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
	_ = c.announce(ctx, fmt.Sprintf("Stopping Minecraft server (%s). Saving world before powering off VM...", reason))

	// Step 1: Try graceful RCON save-all + stop over internal VPC IP with a generous save timeout.
	if info.InternalIP != "" && c.cfg.RCONPassword != "" {
		rconTimeout := c.cfg.RCONStopTimeout
		if rconTimeout < 30*time.Second {
			rconTimeout = 30 * time.Second
		}
		rconCtx, cancel := context.WithTimeout(ctx, rconTimeout)
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
		st := c.mcStatus
		c.mu.Unlock()
		if st != nil {
			c.setPresence(fmt.Sprintf("Minecraft (%d/%d online)", st.OnlinePlayers, st.MaxPlayers))
		} else {
			c.setPresence("Minecraft online")
		}
		return fmt.Errorf("stop VM: %w", err)
	}

	stoppedInfo := gce.InstanceInfo{
		Name:   info.Name,
		Status: gce.StatusTerminated,
	}
	c.resetOfflineState(stoppedInfo)
	_ = c.announce(ctx, fmt.Sprintf("Minecraft VM (`%s`) is now **OFFLINE** (%s). Use `/start` anytime to spin it back up.", c.cfg.MCInstanceName, reason))
	return nil
}

// GetStatus refreshes live GCE and Minecraft SLP status and returns a Snapshot.
// If a start or stop operation is currently in progress, it returns the cached snapshot without
// clobbering lifecycle phase or timers.
func (c *Controller) GetStatus(ctx context.Context) (Snapshot, error) {
	c.mu.Lock()
	if c.opInProgress {
		snap := c.snapshotLocked()
		c.mu.Unlock()
		return snap, nil
	}
	epoch := c.stateEpoch
	c.mu.Unlock()

	info, err := c.vm.GetStatus(ctx)
	if err != nil {
		return Snapshot{}, err
	}

	if !c.epochStillCurrent(epoch) {
		return c.snapshot(), nil
	}

	if info.IsStopped() {
		c.resetOfflineStateIfCurrent(info, epoch)
		return c.snapshot(), nil
	}

	if info.IsRunning() {
		c.handleRunningVM(ctx, info, false, epoch, false)
		c.refreshMinecraftStatus(ctx, info, epoch)
	} else {
		c.mu.Lock()
		if !c.opInProgress && c.stateEpoch == epoch {
			c.vmInfo = info
			if info.Status == gce.StatusStopping {
				c.phase = PhaseStopping
			} else {
				c.phase = PhaseBooting
			}
		}
		c.mu.Unlock()
	}

	return c.snapshot(), nil
}

// RelayDiscordChat forwards a Discord user message into Minecraft via RCON tellraw.
func (c *Controller) RelayDiscordChat(ctx context.Context, author, message string) error {
	c.mu.Lock()
	host := c.vmInfo.InternalIP
	running := c.vmInfo.IsRunning() && c.phase == PhaseOnline
	c.mu.Unlock()

	if !running || host == "" {
		return nil
	}
	rconCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return c.mc.SendDiscordChat(rconCtx, host, author, message)
}

// RunCommand executes a Minecraft server console command over RCON while the VM is online.
func (c *Controller) RunCommand(ctx context.Context, command string) (string, error) {
	c.mu.Lock()
	phase := c.phase
	host := c.vmInfo.InternalIP
	running := c.vmInfo.IsRunning()
	c.mu.Unlock()

	if !running || phase != PhaseOnline || host == "" {
		return "", fmt.Errorf("cannot run command while server is %s", phase)
	}

	rconCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return c.mc.ExecCommand(rconCtx, host, command)
}

// HandleAgentEvent processes a single structured event from mc-agent sequentially.
func (c *Controller) HandleAgentEvent(ctx context.Context, ev events.Event) {
	c.logger.Info("received mc-agent event", "type", ev.Type, "player", ev.Player)

	switch ev.Type {
	case events.TypeVMBoot:
		// External reboot or start: clear session state if no explicit op is running so the
		// IP is guaranteed to be re-announced and a fresh startup grace period begins.
		c.mu.Lock()
		busy := c.opInProgress
		if !busy {
			c.lastAnnouncedIP = ""
			c.readyAnnouncedForIP = ""
			c.mcReady = false
			c.mcStatus = nil
			c.lastBootAt = c.now()
			c.hadPlayersThisSession = false
			c.emptySince = time.Time{}
			c.idleWarned = false
			c.autoStopCooldownUntil = time.Time{}
		}
		epoch := c.stateEpoch
		c.mu.Unlock()

		if !busy {
			c.syncBootedVM(ctx, epoch)
		}

	case events.TypeServerReady:
		c.handleServerReady(ctx)

	case events.TypePlayerJoin:
		c.mu.Lock()
		if !c.opInProgress {
			c.mcReady = true
			c.hadPlayersThisSession = true
			c.emptySince = time.Time{}
			c.idleWarned = false
			c.autoStopCooldownUntil = time.Time{}
		}
		c.mu.Unlock()

		c.sendGameEvent(ctx, ev)
		c.refreshCurrentMC(ctx)

	case events.TypePlayerLeave:
		c.sendGameEvent(ctx, ev)
		c.refreshCurrentMC(ctx)

	case events.TypeChat, events.TypeAdvancement, events.TypeDeath, events.TypeServerStopping:
		c.sendGameEvent(ctx, ev)
	}
}

// PollOnce performs one reconciliation tick: checks GCE state, queries Minecraft SLP,
// updates presence, detects external boots/IP changes, and enforces idle auto-shutdown
// (including when Minecraft crashes or becomes unresponsive past the startup grace period).
func (c *Controller) PollOnce(ctx context.Context) {
	c.mu.Lock()
	if c.opInProgress {
		c.mu.Unlock()
		return
	}
	epoch := c.stateEpoch
	c.mu.Unlock()

	pollCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	info, err := c.vm.GetStatus(pollCtx)
	if err != nil {
		c.logger.Warn("poll gce status failed", "err", err)
		return
	}

	if !c.epochStillCurrent(epoch) {
		return
	}

	if info.IsStopped() {
		c.resetOfflineStateIfCurrent(info, epoch)
		return
	}

	if !info.IsRunning() {
		var presence string
		c.mu.Lock()
		if !c.opInProgress && c.stateEpoch == epoch {
			c.vmInfo = info
			if info.Status == gce.StatusStopping {
				c.phase = PhaseStopping
				presence = "Shutting down server..."
			} else {
				c.phase = PhaseBooting
				presence = "Booting Minecraft VM..."
			}
		}
		c.mu.Unlock()
		if presence != "" {
			c.setPresence(presence)
		}
		return
	}

	// VM is RUNNING
	c.handleRunningVM(pollCtx, info, false, epoch, false)
	status, mcReachable := c.refreshMinecraftStatus(pollCtx, info, epoch)

	// Evaluate idle auto-shutdown even when Minecraft is unreachable (e.g., crash/OOM/hang)
	// once the startup grace period has elapsed so the VM does not bill indefinitely.
	if c.cfg.IdleTimeout <= 0 {
		return
	}

	now := c.now()
	var (
		shouldWarn    bool
		warnRemaining time.Duration
		shouldStop    bool
		stopReason    string
	)

	c.mu.Lock()
	if c.opInProgress || c.stateEpoch != epoch || c.phase != PhaseOnline {
		c.mu.Unlock()
		return
	}

	if mcReachable && status.OnlinePlayers > 0 {
		c.hadPlayersThisSession = true
		c.emptySince = time.Time{}
		c.idleWarned = false
		c.autoStopCooldownUntil = time.Time{}
	} else {
		inGrace := !c.hadPlayersThisSession && now.Sub(c.lastBootAt) < c.cfg.StartupGracePeriod
		if !inGrace {
			if c.emptySince.IsZero() {
				c.emptySince = now
			}
			elapsed := now.Sub(c.emptySince)
			remaining := c.cfg.IdleTimeout - elapsed
			if remaining <= 0 {
				if c.autoStopCooldownUntil.IsZero() || !now.Before(c.autoStopCooldownUntil) {
					// Atomically claim opInProgress under the same lock so a concurrent
					// TypePlayerJoin cannot race between decision and StopServer.
					c.opInProgress = true
					c.stateEpoch++
					shouldStop = true
					if mcReachable {
						stopReason = fmt.Sprintf("auto-shutdown after %s idle with 0 players", formatDuration(c.cfg.IdleTimeout))
					} else {
						stopReason = fmt.Sprintf("auto-shutdown after %s with Minecraft unresponsive", formatDuration(c.cfg.IdleTimeout))
					}
				}
			} else if !c.idleWarned && c.cfg.IdleWarningBefore > 0 && remaining <= c.cfg.IdleWarningBefore {
				shouldWarn = true
				warnRemaining = remaining.Round(time.Second)
			}
		}
	}
	c.mu.Unlock()

	if shouldWarn {
		var warnMsg string
		if mcReachable {
			warnMsg = fmt.Sprintf("No players online — auto-shutdown in **%s** unless someone joins.", formatDuration(warnRemaining))
		} else {
			warnMsg = fmt.Sprintf("Minecraft server has been unresponsive — auto-stopping VM in **%s**.", formatDuration(warnRemaining))
		}
		if err := c.announce(ctx, warnMsg); err == nil {
			c.mu.Lock()
			if c.stateEpoch == epoch {
				c.idleWarned = true
			}
			c.mu.Unlock()
		}
	}

	if shouldStop {
		stopCtx, stopCancel := context.WithTimeout(ctx, 2*time.Minute)
		defer stopCancel()
		if err := c.executeStop(stopCtx, stopReason); err != nil {
			c.logger.Error("auto-shutdown failed", "err", err)
			c.mu.Lock()
			c.autoStopCooldownUntil = c.now().Add(autoStopRetryCooldown)
			c.mu.Unlock()
			if ctx.Err() == nil {
				_ = c.announce(ctx, fmt.Sprintf("Auto-shutdown attempted but encountered an error (will retry in %s): `%v`", formatDuration(autoStopRetryCooldown), err))
			}
		}
	}
}

func (c *Controller) syncBootedVM(ctx context.Context, epoch uint64) {
	syncCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	info, err := c.vm.GetStatus(syncCtx)
	if err != nil {
		c.logger.Warn("syncBootedVM failed", "err", err)
		return
	}
	if info.IsRunning() {
		c.handleRunningVM(syncCtx, info, false, epoch, false)
	}
}

func (c *Controller) handleServerReady(ctx context.Context) {
	c.mu.Lock()
	if c.opInProgress {
		c.mu.Unlock()
		return
	}
	epoch := c.stateEpoch
	c.mu.Unlock()

	syncCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	info, err := c.vm.GetStatus(syncCtx)
	if err != nil {
		c.logger.Warn("handleServerReady failed to fetch GCE status", "err", err)
		return
	}
	if !info.IsRunning() || !c.epochStillCurrent(epoch) {
		return
	}

	c.handleRunningVM(syncCtx, info, false, epoch, false)

	var (
		shouldAnnounceReady bool
		addr                string
	)

	c.mu.Lock()
	if c.opInProgress || c.stateEpoch != epoch {
		c.mu.Unlock()
		return
	}
	c.mcReady = true
	if c.lastBootAt.IsZero() {
		c.lastBootAt = c.now()
	}
	if info.ExternalIP != "" && c.readyAnnouncedForIP != info.ExternalIP {
		shouldAnnounceReady = true
		addr = c.formatAddress(info.ExternalIP)
	}
	c.mu.Unlock()

	if shouldAnnounceReady {
		if err := c.announce(ctx, fmt.Sprintf("Minecraft server is **READY**! Connect at `%s`", addr)); err == nil {
			c.mu.Lock()
			if !c.opInProgress && c.stateEpoch == epoch {
				c.readyAnnouncedForIP = info.ExternalIP
			}
			c.mu.Unlock()
		}
	}
	c.refreshMinecraftStatus(syncCtx, info, epoch)
}

func (c *Controller) handleRunningVM(ctx context.Context, info gce.InstanceInfo, fromStartCmd bool, epoch uint64, allowDuringOp bool) {
	var (
		announceIP string
		mcReady    bool
	)

	c.mu.Lock()
	if (!allowDuringOp && c.opInProgress) || c.stateEpoch != epoch {
		c.mu.Unlock()
		return
	}
	prevPhase := c.phase
	ipChanged := info.ExternalIP != "" && c.lastAnnouncedIP != "" && info.ExternalIP != c.lastAnnouncedIP
	c.vmInfo = info
	c.phase = PhaseOnline
	if prevPhase != PhaseOnline || ipChanged || c.lastBootAt.IsZero() {
		c.lastBootAt = c.now()
		c.hadPlayersThisSession = false
		c.emptySince = time.Time{}
		c.idleWarned = false
		c.autoStopCooldownUntil = time.Time{}
		if ipChanged {
			c.mcReady = false
			c.readyAnnouncedForIP = ""
		}
	}
	if info.ExternalIP != "" && info.ExternalIP != c.lastAnnouncedIP {
		announceIP = info.ExternalIP
	}
	mcReady = c.mcReady
	c.mu.Unlock()

	if announceIP != "" {
		addr := c.formatAddress(announceIP)
		var msg string
		if fromStartCmd {
			msg = fmt.Sprintf("Minecraft VM is **ONLINE**!\nServer IP: `%s`\n*(Waiting for Minecraft server to finish loading...)*", addr)
		} else {
			msg = fmt.Sprintf("Minecraft VM is **ONLINE** with a new IP!\nServer IP: `%s`", addr)
		}
		if err := c.announce(ctx, msg); err == nil {
			c.mu.Lock()
			if (allowDuringOp || !c.opInProgress) && c.stateEpoch == epoch {
				c.lastAnnouncedIP = announceIP
			}
			c.mu.Unlock()
		}
	}

	if !mcReady {
		c.setPresence("Minecraft starting...")
	}
}

func (c *Controller) refreshCurrentMC(ctx context.Context) {
	c.mu.Lock()
	if c.opInProgress {
		c.mu.Unlock()
		return
	}
	info := c.vmInfo
	epoch := c.stateEpoch
	c.mu.Unlock()

	if !info.IsRunning() {
		return
	}
	qCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	c.refreshMinecraftStatus(qCtx, info, epoch)
}

func (c *Controller) refreshMinecraftStatus(ctx context.Context, info gce.InstanceInfo, epoch uint64) (minecraft.ServerStatus, bool) {
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
		valid := !c.opInProgress && c.stateEpoch == epoch
		if valid {
			c.mcStatus = nil
		}
		c.mu.Unlock()
		if valid {
			c.setPresence("Minecraft loading...")
		}
		return minecraft.ServerStatus{}, false
	}

	var announceReadyAddr string

	c.mu.Lock()
	if c.opInProgress || c.stateEpoch != epoch {
		c.mu.Unlock()
		return status, false
	}
	stCopy := status
	c.mcStatus = &stCopy
	c.mcReady = true
	if info.ExternalIP != "" && c.readyAnnouncedForIP != info.ExternalIP {
		announceReadyAddr = c.formatAddress(info.ExternalIP)
	}
	c.mu.Unlock()

	if announceReadyAddr != "" {
		if err := c.announce(ctx, fmt.Sprintf("Minecraft server is **READY**! Connect at `%s`", announceReadyAddr)); err == nil {
			c.mu.Lock()
			if !c.opInProgress && c.stateEpoch == epoch {
				c.readyAnnouncedForIP = info.ExternalIP
			}
			c.mu.Unlock()
		}
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
			// Minecraft closes its TCP listener before finishing player/chunk disk flushes.
			// Wait a short settle window before triggering GCE VM power-off.
			settle := defaultPostCloseSettle
			if half := timeout / 2; settle > half {
				settle = half
			}
			if settle > 0 {
				select {
				case <-ctx.Done():
				case <-time.After(settle):
				}
			}
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

func (c *Controller) epochStillCurrent(epoch uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.opInProgress && c.stateEpoch == epoch
}

func (c *Controller) resetOfflineStateIfCurrent(info gce.InstanceInfo, epoch uint64) {
	c.mu.Lock()
	if c.opInProgress || c.stateEpoch != epoch {
		c.mu.Unlock()
		return
	}
	c.resetOfflineLocked(info)
	c.mu.Unlock()
	c.setPresence("Offline — /start to play")
}

func (c *Controller) resetOfflineState(info gce.InstanceInfo) {
	c.mu.Lock()
	c.resetOfflineLocked(info)
	c.mu.Unlock()
	c.setPresence("Offline — /start to play")
}

func (c *Controller) resetOfflineLocked(info gce.InstanceInfo) {
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
	c.autoStopCooldownUntil = time.Time{}
}

func (c *Controller) snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshotLocked()
}

func (c *Controller) snapshotLocked() Snapshot {
	port := c.cfg.MCPort
	if port <= 0 {
		port = 25565
	}
	snap := Snapshot{
		Phase:   c.phase,
		VM:      c.vmInfo,
		MCReady: c.mcReady,
		MCPort:  port,
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
	port := c.cfg.MCPort
	if port <= 0 {
		port = 25565
	}
	return fmt.Sprintf("%s:%d", ip, port)
}

func (c *Controller) announce(ctx context.Context, msg string) error {
	if c.notifier == nil {
		return nil
	}
	return c.notifier.Announce(ctx, msg)
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

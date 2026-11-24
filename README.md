# GCE Minecraft Controller Discord Bot

A lightweight **Go** controller system built for Google Compute Engine (GCE):

- **`mc-bot`** — runs 24/7 on a small GCE controller VM (e.g., `e2-micro`, ~15–25 MB RAM). Starts/stops your on-demand Minecraft VM, announces the **new ephemeral external IP** in Discord on every single boot, queries live Minecraft telemetry over the private VPC, relays Discord chat into Minecraft via RCON, and automatically shuts the VM down after an idle timeout when `0` players are online.
- **`mc-agent`** — a tiny (~6 MB RAM) zero-dependency Go binary running alongside your Minecraft Java Edition server on `mc-vm`. Tails `logs/latest.log` across log rotations and pushes server ready alerts, join/leave events, in-game chat, advancements, and deaths over the internal VPC to `mc-bot`.

Works with **Vanilla, Paper, Fabric, and Forge** Java Edition servers without any server-side mods or plugins.

---

## Features

- **Slash Commands** (open to everyone in the server):
  - `/start` — Boots `mc-vm`, polls GCE until the fresh ephemeral IP is assigned, and posts `<ip>:25565` in Discord.
  - `/stop` — Sends RCON `save-all flush` + `stop`, waits for Minecraft to save cleanly, then terminates `mc-vm`.
  - `/status` — Rich Discord embed showing VM status, current IP, MOTD, version, player count + names, internal ping, and idle auto-shutdown countdown.
  - `/players` — Lists currently online players.
- **Guaranteed Ephemeral IP Posting**: Every time `mc-vm` starts (via `/start` or even manually from the GCP Console), `mc-bot` posts the newly assigned `<ip>:25565` in the announcement channel, followed by a ready alert once Minecraft finishes loading.
- **Idle Auto-Shutdown**:
  - Configurable startup grace period (default `10m`) so players have time to launch their client.
  - Configurable idle timeout (default `15m`) with a pre-shutdown Discord warning (`2m` before stopping).
  - Always flushes & saves the world via RCON before powering off the GCE VM.
- **Two-Way Chat & Game Events** (`#mc-chat`):
  - **MC -> Discord**: Join/leave messages, in-game chat, advancements, and deaths (mention-escaped so players cannot ping `@everyone`).
  - **Discord -> MC**: Messages typed in the configured chat channel are sent via JSON `tellraw @a` (safe against command injection).

---

## Building the Binaries

Requires Go 1.24+ (auto-downloads the Go toolchain required by `go.mod`).

```bash
make build
```

This produces two static Linux binaries with zero external runtime dependencies:
- `bin/mc-bot` (deploy to `bot-vm`)
- `bin/mc-agent` (deploy to `mc-vm`)

Run tests:

```bash
make test
```

---

## Setup & Deployment Guide

### 1. Create the Discord Bot

1. Go to the [Discord Developer Portal](https://discord.com/developers/applications) and create an application.
2. Under **Bot**, create a bot user and copy its **Token**.
3. If you want live **Discord -> Minecraft chat** in `#mc-chat`, enable **Message Content Intent** under **Privileged Gateway Intents**.
4. Under **OAuth2 -> URL Generator**, select scopes `bot` and `applications.commands`, and permissions:
   - `View Channels`, `Send Messages`, `Embed Links`
5. Invite the bot to your Discord server and copy your target channel ID(s) (and optionally your Server/Guild ID for instant slash command updates).

### 2. Configure GCP IAM & VPC Firewall

Review and run [`deploy/gcp_setup.sh`](deploy/gcp_setup.sh) from your local machine or Cloud Shell:

```bash
PROJECT_ID=my-gcp-project ZONE=us-central1-a BOT_VM=bot-vm MC_VM=mc-vm bash deploy/gcp_setup.sh
```

This creates:
- A dedicated Service Account with a custom role limited to `get`/`start`/`stop` on `mc-vm`.
- Firewall rules exposing only `tcp:25565` publicly to `mc-vm`, while keeping RCON (`25575`) and the webhook (`8080`) strictly inside the private VPC.

> No JSON service account key files are needed—`mc-bot` authenticates automatically through the GCE metadata server.

### 3. Enable RCON on `mc-vm`

In your Minecraft server's `server.properties`:

```properties
enable-rcon=true
rcon.port=25575
rcon.password=your-strong-rcon-password
broadcast-rcon-to-ops=false
```

### 4. Deploy `mc-bot` on `bot-vm` (24/7 VM)

```bash
# Copy binary and systemd service to bot-vm
gcloud compute scp bin/mc-bot deploy/mc-controller-bot.service bot-vm:/tmp/ --zone=us-central1-a

# SSH into bot-vm and install
gcloud compute ssh bot-vm --zone=us-central1-a --command '
  sudo useradd --system --no-create-home --shell /usr/sbin/nologin mcbot || true
  sudo install -m 0755 /tmp/mc-bot /usr/local/bin/mc-bot
  sudo mkdir -p /opt/mc-bot /etc/mc-bot
  sudo install -m 0644 /tmp/mc-controller-bot.service /etc/systemd/system/mc-controller-bot.service
'
```

Create `/etc/mc-bot/bot.env` on `bot-vm` based on [`.env.example`](.env.example), lock down permissions (`sudo chmod 600 /etc/mc-bot/bot.env && sudo chown mcbot:mcbot /etc/mc-bot/bot.env`), and enable the service:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now mc-controller-bot
sudo journalctl -u mc-controller-bot -f
```

### 5. Deploy `mc-agent` on `mc-vm` (Minecraft VM)

```bash
# Copy binary and systemd service to mc-vm
gcloud compute scp bin/mc-agent deploy/mc-companion-agent.service mc-vm:/tmp/ --zone=us-central1-a

# SSH into mc-vm and install
gcloud compute ssh mc-vm --zone=us-central1-a --command '
  sudo install -m 0755 /tmp/mc-agent /usr/local/bin/mc-agent
  sudo mkdir -p /etc/mc-agent
  sudo install -m 0644 /tmp/mc-companion-agent.service /etc/systemd/system/mc-companion-agent.service
'
```

Create `/etc/mc-agent/agent.env` on `mc-vm`:

```env
MC_LOG_PATH=/opt/minecraft/logs/latest.log
BOT_WEBHOOK_URL=http://bot-vm.c.my-gcp-project.internal:8080/events
WEBHOOK_SECRET=same-secret-as-bot-vm
```

Adjust `User=`/`Group=` in `/etc/systemd/system/mc-companion-agent.service` if your Minecraft server runs under a different Linux user, then enable it:

```bash
sudo chmod 600 /etc/mc-agent/agent.env
sudo systemctl daemon-reload
sudo systemctl enable --now mc-companion-agent
sudo journalctl -u mc-companion-agent -f
```

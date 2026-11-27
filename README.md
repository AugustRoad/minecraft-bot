# GCE Minecraft Controller Discord Bot

A lightweight **Go** controller system built for Google Compute Engine (GCE):

- **`mc-bot`** — runs 24/7 on a small GCE controller VM (e.g., `e2-micro`, ~15–25 MB RAM). Starts/stops your on-demand Minecraft VM, announces the **new ephemeral external IP** in Discord on every single boot, queries live Minecraft telemetry over the private VPC, relays Discord chat and admin console commands into Minecraft via RCON, and automatically shuts the VM down after an idle timeout when `0` players are online.
- **`mc-agent`** — a tiny (~6 MB RAM) zero-dependency Go binary running alongside your Minecraft Java Edition server on `mc-vm`. Tails `logs/latest.log` across log rotations and pushes server ready alerts, join/leave events, in-game chat, advancements, and deaths over the internal VPC to `mc-bot`.

Works with **Vanilla, Paper, Fabric, Forge, and NeoForge** Java Edition servers without any server-side mods or plugins.

---

## Features

- **Slash Commands**:
  - `/start` — Boots `mc-vm`, polls GCE until the fresh ephemeral IP is assigned, and posts `<ip>:25565` in Discord.
  - `/stop` — Sends RCON `save-all flush` + `stop`, waits for Minecraft to save cleanly, then terminates `mc-vm`.
  - `/status` — Rich Discord embed showing VM status, current IP, MOTD, version, player count + names, internal ping, and idle auto-shutdown countdown.
  - `/players` — Lists currently online players.
  - `/cmd <command>` *(Discord Administrators only)* — Executes a Minecraft server console command over RCON (e.g. `op Steve`, `gamerule keepInventory true`, `whitelist add Friend`).
- **Guaranteed Ephemeral IP Posting**: Every time `mc-vm` starts (via `/start` or even manually from the GCP Console), `mc-bot` posts the newly assigned `<ip>:25565` in the announcement channel, followed by a ready alert once Minecraft finishes loading.
- **Idle Auto-Shutdown**:
  - Configurable startup grace period (default `15m` in `.env.example` for modded Minecraft) so players have time to launch their client.
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

## Initial Setup & Deployment

### 1. Create the Discord Bot

1. Go to the [Discord Developer Portal](https://discord.com/developers/applications) and create an application.
2. Under **Bot**, create a bot user and copy its **Token**.
3. If you want live **Discord -> Minecraft chat** in `#mc-chat`, enable **Message Content Intent** under **Privileged Gateway Intents**.
4. Under **OAuth2 -> URL Generator**, select scopes `bot` and `applications.commands`, and permissions:
   - `View Channels`, `Send Messages`, `Embed Links`
5. Invite the bot to your Discord server and copy your target channel ID(s) and Server (Guild) ID.

### 2. Configure GCP IAM & VPC Firewall

Review and run [`deploy/gcp_setup.sh`](deploy/gcp_setup.sh) from your local machine or GCP Cloud Shell:

```bash
PROJECT_ID=my-gcp-project ZONE=us-central1-a BOT_VM=bot-vm MC_VM=mc-vm bash deploy/gcp_setup.sh
```

Then attach the Service Account to `bot-vm` (requires a brief stop/start):

```bash
gcloud compute instances stop bot-vm --zone=us-central1-a
gcloud compute instances set-service-account bot-vm --zone=us-central1-a \
  --service-account="mc-discord-bot@${PROJECT_ID}.iam.gserviceaccount.com" \
  --scopes=https://www.googleapis.com/auth/compute
gcloud compute instances start bot-vm --zone=us-central1-a
```

> No JSON service account key files are needed—`mc-bot` authenticates automatically through the GCE metadata server.

### 3. Enable RCON on `mc-vm`

In `/opt/minecraft/server.properties`:

```properties
enable-rcon=true
rcon.port=25575
rcon.password=your-strong-rcon-password
broadcast-rcon-to-ops=false
```

### 4. Deploy `mc-bot` on `bot-vm` (24/7 VM)

```bash
gcloud compute scp bin/mc-bot deploy/mc-controller-bot.service bot-vm:/tmp/ --zone=us-central1-a

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
gcloud compute scp bin/mc-agent deploy/mc-companion-agent.service mc-vm:/tmp/ --zone=us-central1-a

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

Enable and start the service:

```bash
sudo chown minecraft:minecraft /etc/mc-agent/agent.env
sudo chmod 600 /etc/mc-agent/agent.env
sudo systemctl daemon-reload
sudo systemctl enable --now mc-companion-agent
sudo journalctl -u mc-companion-agent -f
```

---

## Maintenance & Operations Guide

> In all commands below, replace `YOUR_PROJECT_ID` and `us-central1-a` with your GCP project ID and zone.

### 1. How to Change the Discord Server (or Channels)

1. **Invite the bot to the new Discord server** `[Browser]`:
   - Go to [Discord Developer Portal](https://discord.com/developers/applications) -> your app -> **OAuth2 -> URL Generator**.
   - Check scopes `bot` + `applications.commands` and permissions `View Channels`, `Send Messages`, `Embed Links`.
   - Open the generated URL and add the bot to the new server.
   - *(Optional)* Kick the bot from the old Discord server.
2. **Copy the new Discord IDs** `[Discord]` (with **User Settings -> Advanced -> Developer Mode** enabled):
   - Right-click the new **Server icon** -> **Copy Server ID** (`DISCORD_GUILD_ID`).
   - Right-click the new **Announcements channel** -> **Copy Channel ID** (`DISCORD_ANNOUNCE_CHANNEL_ID`).
   - Right-click the new **Live chat channel** -> **Copy Channel ID** (`DISCORD_CHAT_CHANNEL_ID` — can be the same channel ID, or blank to disable chat bridging).
3. **Update `/etc/mc-bot/bot.env` and restart `mc-bot`** `[GCP Cloud Shell]` -> `[SSH on bot-vm]`:
   ```bash
   gcloud compute ssh bot-vm --project=YOUR_PROJECT_ID --zone=us-central1-a
   ```
   Inside `bot-vm`:
   ```bash
   sudo nano /etc/mc-bot/bot.env
   ```
   Update these three lines, save (`Ctrl+O`, `Enter`, `Ctrl+X`), and restart:
   ```env
   DISCORD_GUILD_ID=NEW_SERVER_ID
   DISCORD_ANNOUNCE_CHANNEL_ID=NEW_ANNOUNCE_CHANNEL_ID
   DISCORD_CHAT_CHANNEL_ID=NEW_CHAT_CHANNEL_ID
   ```
   ```bash
   sudo systemctl restart mc-controller-bot
   sudo journalctl -u mc-controller-bot -n 30 --no-pager
   ```
   Slash commands register immediately in the new server on startup.

---

### 2. How to Update the Minecraft Server Modpack Version

Always back up your world and server config before updating a modpack.

1. **Start `mc-vm` (if stopped) and SSH in** `[GCP Cloud Shell]`:
   ```bash
   gcloud compute instances start mc-vm --project=YOUR_PROJECT_ID --zone=us-central1-a
   gcloud compute ssh mc-vm --project=YOUR_PROJECT_ID --zone=us-central1-a
   ```
2. **Stop the Minecraft service and take a full backup** `[SSH on mc-vm]`:
   ```bash
   sudo systemctl stop minecraft

   # Back up the entire server directory (world, configs, server.properties, ops, etc.)
   sudo mkdir -p /opt/mc-backups
   sudo tar -czf "/opt/mc-backups/minecraft-backup-$(date +%F-%H%M).tar.gz" -C /opt minecraft
   ls -lh /opt/mc-backups
   ```
3. **Download (or upload) the new Server Pack `.zip`** `[SSH on mc-vm]`:
   - If the modpack publishes on GitHub Releases (e.g. TerraFirmaGreg-Modern):
     ```bash
     curl -L -o /tmp/new-serverpack.zip "https://github.com/.../new-serverpack.zip"
     ```
   - Or upload via the GCP Console browser SSH **Upload file** button (saves to `~/new-serverpack.zip`).
4. **Clean old modpack files while keeping your world & server settings** `[SSH on mc-vm]`:
   ```bash
   # Save your current server.properties so RCON password and world settings aren't overwritten
   sudo cp /opt/minecraft/server.properties /tmp/server.properties.bak

   # Remove old modpack folders that should be replaced cleanly (do NOT delete world/!)
   sudo rm -rf /opt/minecraft/mods /opt/minecraft/config /opt/minecraft/defaultconfigs /opt/minecraft/kubejs /opt/minecraft/libraries

   # Extract the new server pack into /opt/minecraft
   sudo -u minecraft unzip -o /tmp/new-serverpack.zip -d /opt/minecraft
   sudo rm /tmp/new-serverpack.zip

   # Restore your server.properties (preserves RCON password, MOTD, level-name, etc.)
   sudo cp /tmp/server.properties.bak /opt/minecraft/server.properties
   sudo chown -R minecraft:minecraft /opt/minecraft
   ```
   *(Check if the new modpack version changed the launcher jar/script or requires a newer Java version. If so, update `ExecStart=` in `/etc/systemd/system/minecraft.service` and run `sudo systemctl daemon-reload`.)*
5. **Start Minecraft and watch the logs** `[SSH on mc-vm]`:
   ```bash
   sudo systemctl start minecraft
   sudo journalctl -u minecraft -f
   ```
   - **Rollback if something breaks:** `sudo systemctl stop minecraft && sudo rm -rf /opt/minecraft && sudo tar -xzf /opt/mc-backups/minecraft-backup-YYYY-MM-DD-HHMM.tar.gz -C /opt && sudo systemctl start minecraft`.

---

### 3. How to Change, Reset, or Upload a Minecraft World

First, SSH into `mc-vm` and stop Minecraft (`sudo systemctl stop minecraft`).

#### Option A: Start a Brand-New World (Keep the Old World as a Backup)
```bash
# Rename the existing world folder so it's safely archived
sudo mv /opt/minecraft/world "/opt/minecraft/world-old-$(date +%F)"

# Start Minecraft — it will automatically generate a brand-new world
sudo systemctl start minecraft
sudo journalctl -u minecraft -f
```
*(Optional: To use a specific seed, edit `level-seed=` in `/opt/minecraft/server.properties` before starting.)*

#### Option B: Upload an Existing World (e.g., from Single-Player or Another Host)
1. Zip your world folder on your PC (so the `.zip` contains `level.dat`, `region/`, etc.).
2. Open GCP Console -> **Compute Engine -> VM instances** -> click **SSH** on `mc-vm` -> click **Upload file** and upload `my-world.zip` (lands in `~/my-world.zip`).
3. Inside **`[SSH on mc-vm]`**:
   ```bash
   sudo systemctl stop minecraft
   sudo mv /opt/minecraft/world "/opt/minecraft/world-backup-$(date +%F)" || true
   sudo mkdir -p /opt/minecraft/world
   sudo unzip ~/my-world.zip -d /opt/minecraft/world
   sudo chown -R minecraft:minecraft /opt/minecraft/world
   ```
   Verify `level.dat` sits directly at `/opt/minecraft/world/level.dat`:
   ```bash
   ls -la /opt/minecraft/world/level.dat
   ```
   *(If unzip created a nested folder like `/opt/minecraft/world/MySave/level.dat`, move its contents up: `sudo sh -c 'mv /opt/minecraft/world/MySave/* /opt/minecraft/world/ && rmdir /opt/minecraft/world/MySave'` and re-run `sudo chown -R minecraft:minecraft /opt/minecraft/world`.)*
   Then start Minecraft:
   ```bash
   sudo systemctl start minecraft
   sudo journalctl -u minecraft -f
   ```

#### Option C: Switch Between Multiple Worlds on Disk
You can keep multiple world folders inside `/opt/minecraft/` (e.g. `world`, `world-creative`, `world-season2`) and switch between them by editing `level-name=` in `/opt/minecraft/server.properties`:
```bash
sudo systemctl stop minecraft
sudo -u minecraft nano /opt/minecraft/server.properties
# Change: level-name=world-season2
sudo systemctl start minecraft
```

---

### 4. How to Update the Discord Bot (`mc-bot`) & Companion Agent (`mc-agent`)

Run these commands from **`[GCP Cloud Shell]`**.

#### Update `mc-bot` (on `bot-vm`)
```bash
cd ~/minecraft-bot
git pull origin main
make build-bot

gcloud compute scp bin/mc-bot bot-vm:/tmp/mc-bot \
  --project=YOUR_PROJECT_ID \
  --zone=us-central1-a

gcloud compute ssh bot-vm \
  --project=YOUR_PROJECT_ID \
  --zone=us-central1-a \
  --command 'sudo systemctl stop mc-controller-bot && sudo install -m 0755 /tmp/mc-bot /usr/local/bin/mc-bot && sudo systemctl start mc-controller-bot && sudo systemctl status mc-controller-bot --no-pager'
```

#### Update `mc-agent` (on `mc-vm` — only needed if `cmd/agent` or `internal/logtail` changed)
*(Make sure `mc-vm` is running first.)*
```bash
cd ~/minecraft-bot
git pull origin main
make build-agent

gcloud compute scp bin/mc-agent mc-vm:/tmp/mc-agent \
  --project=YOUR_PROJECT_ID \
  --zone=us-central1-a

gcloud compute ssh mc-vm \
  --project=YOUR_PROJECT_ID \
  --zone=us-central1-a \
  --command 'sudo systemctl stop mc-companion-agent && sudo install -m 0755 /tmp/mc-agent /usr/local/bin/mc-agent && sudo systemctl start mc-companion-agent && sudo systemctl status mc-companion-agent --no-pager'
```

---

### 5. Automatic 15-Minute World Backups (Keep Latest 2)

[`deploy/mc-backup.py`](deploy/mc-backup.py), [`deploy/mc-backup.service`](deploy/mc-backup.service), and [`deploy/mc-backup.timer`](deploy/mc-backup.timer) run an RCON-safe backup (`save-off` -> `save-all flush` -> `tar` -> `save-on`) every 15 minutes while `mc-vm` is running and automatically keep only the **2 newest** backups in `/opt/mc-backups`.

#### Install from `[GCP Cloud Shell]`
```bash
cd ~/minecraft-bot
gcloud compute scp deploy/mc-backup.py deploy/mc-backup.service deploy/mc-backup.timer mc-vm:/tmp/ \
  --project=YOUR_PROJECT_ID \
  --zone=us-central1-a

gcloud compute ssh mc-vm \
  --project=YOUR_PROJECT_ID \
  --zone=us-central1-a \
  --command '
    sudo install -m 0755 /tmp/mc-backup.py /usr/local/bin/mc-backup.py &&
    sudo install -m 0644 /tmp/mc-backup.service /etc/systemd/system/mc-backup.service &&
    sudo install -m 0644 /tmp/mc-backup.timer /etc/systemd/system/mc-backup.timer &&
    sudo systemctl daemon-reload &&
    sudo systemctl enable --now mc-backup.timer &&
    sudo systemctl start mc-backup.service &&
    ls -lh /opt/mc-backups
  '
```

#### Restore a Backup `[SSH on mc-vm]`
```bash
ls -lh /opt/mc-backups
sudo systemctl stop minecraft
sudo rm -rf /opt/minecraft/world
sudo tar -xzf /opt/mc-backups/world-YYYY-MM-DD-HHMMSS.tar.gz -C /opt/minecraft
sudo chown -R minecraft:minecraft /opt/minecraft/world
sudo systemctl start minecraft
```

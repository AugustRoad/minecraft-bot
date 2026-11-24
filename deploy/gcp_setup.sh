#!/usr/bin/env bash
# Reference GCP setup script for least-privilege IAM and VPC firewall rules.
# Review and customize the variables below before running.
set -euo pipefail

PROJECT_ID="${PROJECT_ID:-my-gcp-project}"
ZONE="${ZONE:-us-central1-a}"
NETWORK="${NETWORK:-default}"
BOT_VM="${BOT_VM:-bot-vm}"
MC_VM="${MC_VM:-mc-vm}"
SA_NAME="${SA_NAME:-mc-discord-bot}"
SA_EMAIL="${SA_NAME}@${PROJECT_ID}.iam.gserviceaccount.com"
INSTANCE_ROLE_ID="mcVmInstanceController"
OPS_ROLE_ID="mcZoneOperationsReader"

echo "==> Setting project to ${PROJECT_ID}"
gcloud config set project "${PROJECT_ID}"

echo "==> Creating dedicated Service Account for ${BOT_VM}"
gcloud iam service-accounts create "${SA_NAME}" \
  --display-name="Minecraft Discord Controller Bot" || true

echo "==> Creating least-privilege custom IAM role for VM control (${INSTANCE_ROLE_ID})"
gcloud iam roles create "${INSTANCE_ROLE_ID}" \
  --project="${PROJECT_ID}" \
  --title="Minecraft VM Instance Controller" \
  --description="Allows reading, starting, and stopping a specific GCE VM instance" \
  --permissions="compute.instances.get,compute.instances.start,compute.instances.stop" \
  --stage="GA" || true

echo "==> Creating minimal custom IAM role for waiting on zone operations (${OPS_ROLE_ID})"
gcloud iam roles create "${OPS_ROLE_ID}" \
  --project="${PROJECT_ID}" \
  --title="Minecraft Zone Operations Reader" \
  --description="Allows polling Compute Engine zone operation completion" \
  --permissions="compute.zoneOperations.get" \
  --stage="GA" || true

echo "==> Binding VM control role directly on ${MC_VM}"
gcloud compute instances add-iam-policy-binding "${MC_VM}" \
  --zone="${ZONE}" \
  --member="serviceAccount:${SA_EMAIL}" \
  --role="projects/${PROJECT_ID}/roles/${INSTANCE_ROLE_ID}"

echo "==> Binding zone operations read role at the project level (required by op.Wait)"
gcloud projects add-iam-policy-binding "${PROJECT_ID}" \
  --member="serviceAccount:${SA_EMAIL}" \
  --role="projects/${PROJECT_ID}/roles/${OPS_ROLE_ID}" \
  --condition=None

echo "==> Adding VPC network tags to ${BOT_VM} and ${MC_VM}"
gcloud compute instances add-tags "${BOT_VM}" --zone="${ZONE}" --tags=discord-bot
gcloud compute instances add-tags "${MC_VM}" --zone="${ZONE}" --tags=minecraft-server

echo "==> Creating VPC firewall rules"
# 1. Public Minecraft gameplay port (TCP 25565) -> mc-vm only
gcloud compute firewall-rules create allow-mc-public \
  --network="${NETWORK}" \
  --direction=INGRESS \
  --action=ALLOW \
  --rules=tcp:25565 \
  --source-ranges=0.0.0.0/0 \
  --target-tags=minecraft-server || true

# 2. Internal bot-vm -> mc-vm for SLP (25565) and RCON (25575)
gcloud compute firewall-rules create allow-bot-to-mc-internal \
  --network="${NETWORK}" \
  --direction=INGRESS \
  --action=ALLOW \
  --rules=tcp:25565,tcp:25575 \
  --source-tags=discord-bot \
  --target-tags=minecraft-server || true

# 3. Internal mc-vm -> bot-vm for companion agent webhook (8080)
gcloud compute firewall-rules create allow-mc-to-bot-internal \
  --network="${NETWORK}" \
  --direction=INGRESS \
  --action=ALLOW \
  --rules=tcp:8080 \
  --source-tags=minecraft-server \
  --target-tags=discord-bot || true

echo ""
echo "Next step: attach ${SA_EMAIL} to ${BOT_VM} (must be stopped briefly to change SA):"
echo "  gcloud compute instances stop ${BOT_VM} --zone=${ZONE}"
echo "  gcloud compute instances set-service-account ${BOT_VM} --zone=${ZONE} \\"
echo "    --service-account=${SA_EMAIL} --scopes=https://www.googleapis.com/auth/compute"
echo "  gcloud compute instances start ${BOT_VM} --zone=${ZONE}"

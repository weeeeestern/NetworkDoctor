#!/usr/bin/env bash
# One-shot host setup for the LLM gateway VM (Ubuntu 24.04). Run as root:
#   sudo bash install-docker.sh <user-that-runs-docker>
# Idempotent: safe to re-run.
set -euo pipefail

RUN_USER="${1:-${SUDO_USER:-litellm}}"

echo "== apt prerequisites =="
apt-get update -qq
apt-get install -y -qq ca-certificates curl gnupg qemu-guest-agent jq
systemctl enable --now qemu-guest-agent >/dev/null 2>&1 || true

echo "== docker repo =="
install -m 0755 -d /etc/apt/keyrings
if [ ! -f /etc/apt/keyrings/docker.gpg ]; then
  curl -fsSL https://download.docker.com/linux/ubuntu/gpg | gpg --dearmor -o /etc/apt/keyrings/docker.gpg
  chmod a+r /etc/apt/keyrings/docker.gpg
fi
. /etc/os-release
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu ${VERSION_CODENAME} stable" \
  > /etc/apt/sources.list.d/docker.list
apt-get update -qq
apt-get install -y -qq docker-ce docker-ce-cli containerd.io docker-compose-plugin
systemctl enable --now docker
usermod -aG docker "$RUN_USER"

echo "== root LV: use the rest of the volume group (Ubuntu installer leaves it unused) =="
if command -v lvextend >/dev/null && lvs --noheadings -o lv_path 2>/dev/null | grep -q ubuntu-lv; then
  lvextend -l +100%FREE -r /dev/ubuntu-vg/ubuntu-lv >/dev/null 2>&1 || true
fi

echo "== result =="
docker --version
docker compose version
df -h / | tail -1
echo "OK: log out and back in so '$RUN_USER' picks up the docker group."

#!/usr/bin/env bash
# Runs as root via cloud-init runcmd during the ONE-TIME image bake boot.
# Installs Docker + the actions runner, then powers off. The host watches
# the serial console for BAKE-OK; any failure means no sentinel and the
# bake is rejected.
set -euxo pipefail
exec >/dev/console 2>&1

# Always power off when this script exits, success or failure. The host's
# only success signal is the BAKE-OK sentinel on the serial console; on any
# failure `set -e` aborts before BAKE-OK is printed, so powering off here
# makes a failed bake end in seconds (host sees no sentinel, fails fast)
# instead of hanging until the host's 30-minute timeout.
poweroff_on_exit() {
  rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "BAKE-FAILED rc=$rc"
  fi
  poweroff -f
}
trap poweroff_on_exit EXIT

# Written by the bake user-data: VERSION, TARBALL_URL, TARBALL_SHA256.
# shellcheck disable=SC1091
source /run/bake-env

export DEBIAN_FRONTEND=noninteractive

# The cloud image ships unattended-upgrades plus the apt-daily timers; in
# cloned job VMs they grab the apt/dpkg lock at boot and break jobs running
# `apt-get`. Guest OS updates land via image refresh (the dist-upgrade
# below), never inside a job VM. Stop the services too: the timers may have
# already fired on this bake boot, and a running apt-daily would hold the
# lock against the purge and everything after it.
systemctl mask --now apt-daily.timer apt-daily-upgrade.timer
systemctl stop apt-daily.service apt-daily-upgrade.service unattended-upgrades.service
apt-get purge -y unattended-upgrades

useradd --create-home --shell /bin/bash runner
# Passwordless sudo matches GitHub-hosted images; the disposable VM is the
# trust boundary, and runner is in the docker group (root-equivalent) anyway.
echo 'runner ALL=(ALL) NOPASSWD:ALL' >/etc/sudoers.d/runner
chmod 0440 /etc/sudoers.d/runner

apt-get update
apt-get dist-upgrade -y
apt-get install -y --no-install-recommends \
  git curl ca-certificates jq build-essential sudo unzip
echo 'APT::Get::Assume-Yes "true";' >/etc/apt/apt.conf.d/90assume-yes

# Docker CE from Docker's official repo — same distribution as the docker
# backend's dind image, and the only one shipping the buildx + compose v2
# CLI plugins (Ubuntu's docker.io has neither). Codename and arch are read
# from the guest so a non-noble imagebake.image_url keeps working.
install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
# shellcheck disable=SC1091
. /etc/os-release
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu ${VERSION_CODENAME} stable" \
  >/etc/apt/sources.list.d/docker.list
apt-get update
apt-get install -y --no-install-recommends \
  docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
usermod -aG docker runner
systemctl enable docker

# Plugin discovery must work or the bake fails (no BAKE-OK): these are the
# commands jobs will run.
docker buildx version
docker compose version

mkdir -p /opt/actions-runner
curl -fsSL "$TARBALL_URL" -o /tmp/runner.tar.gz
if [ -n "$TARBALL_SHA256" ]; then
  echo "$TARBALL_SHA256  /tmp/runner.tar.gz" | sha256sum -c -
fi
tar -xzf /tmp/runner.tar.gz -C /opt/actions-runner
rm /tmp/runner.tar.gz
/opt/actions-runner/bin/installdependencies.sh
chown -R runner:runner /opt/actions-runner

install -m 0755 /run/run-one-job /usr/local/bin/run-one-job

# Make the image boot as a fresh instance every time it's cloned.
cloud-init clean --logs --machine-id

echo "BAKE-OK"
# poweroff is handled by the EXIT trap above.

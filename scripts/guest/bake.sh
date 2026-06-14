#!/usr/bin/env bash
# Runs as root via cloud-init runcmd during the ONE-TIME image bake boot.
# Installs Docker + the actions runner, then powers off. The host watches
# the serial console for BAKE-OK; any failure means no sentinel and the
# bake is rejected.
set -euxo pipefail
exec >/dev/console 2>&1

# Written by the bake user-data: VERSION, TARBALL_URL, TARBALL_SHA256.
# shellcheck disable=SC1091
source /run/bake-env

export DEBIAN_FRONTEND=noninteractive

useradd --create-home --shell /bin/bash runner
# Passwordless sudo matches GitHub-hosted images; the disposable VM is the
# trust boundary, and runner is in the docker group (root-equivalent) anyway.
echo 'runner ALL=(ALL) NOPASSWD:ALL' >/etc/sudoers.d/runner
chmod 0440 /etc/sudoers.d/runner

apt-get update
apt-get install -y --no-install-recommends \
  git curl ca-certificates jq build-essential sudo docker.io unzip
echo 'APT::Get::Assume-Yes "true"' >/etc/apt/apt.conf.d/90assume-yes
usermod -aG docker runner
systemctl enable docker

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
poweroff

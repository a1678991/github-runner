# Runner images for the docker backend, built locally by
# `github-qemu-runner refresh-image`; never pushed to a registry.
#   --target dind -> ghq-runner-base:latest  (Docker Engine inside, for
#       isolation: gvisor pools; run with --runtime runsc --privileged)
#   --target slim -> ghq-runner-slim:latest  (no Docker Engine, for
#       isolation: seccomp pools; native runc, no --privileged)
FROM ubuntu:24.04 AS base
ARG RUNNER_VERSION
ARG RUNNER_TARBALL_URL
ARG RUNNER_TARBALL_SHA256

LABEL org.opencontainers.image.title="ghq-runner-base" \
      org.opencontainers.image.version="${RUNNER_VERSION}"

RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates curl git sudo unzip && \
    echo 'APT::Get::Assume-Yes "true";' > /etc/apt/apt.conf.d/90assume-yes && \
    rm -rf /var/lib/apt/lists/*

# uid 1001: ubuntu:24.04 ships an "ubuntu" user at 1000. Passwordless sudo
# matches GitHub-hosted images; the disposable sandbox is the trust
# boundary.
RUN useradd --create-home --uid 1001 runner && \
    echo 'runner ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/runner && \
    chmod 0440 /etc/sudoers.d/runner

RUN mkdir -p /opt/actions-runner && cd /opt/actions-runner && \
    curl -fsSL -o runner.tar.gz "$RUNNER_TARBALL_URL" && \
    { if [ -z "$RUNNER_TARBALL_SHA256" ]; then echo "WARNING: RUNNER_TARBALL_SHA256 empty; skipping tarball integrity check" >&2; else echo "$RUNNER_TARBALL_SHA256  runner.tar.gz" | sha256sum -c -; fi; } && \
    tar xzf runner.tar.gz && rm runner.tar.gz && \
    apt-get update && ./bin/installdependencies.sh && rm -rf /var/lib/apt/lists/* && \
    chown -R runner:runner /opt/actions-runner

# DinD variant: Docker Engine + entrypoint that boots the inner dockerd.
# runner joins the docker group (root-equivalent inside the container).
FROM base AS dind
RUN install -m 0755 -d /etc/apt/keyrings && \
    curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc && \
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu noble stable" \
        > /etc/apt/sources.list.d/docker.list && \
    apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        docker-ce docker-ce-cli containerd.io docker-buildx-plugin && \
    rm -rf /var/lib/apt/lists/* && \
    usermod -aG docker runner

COPY --chmod=0755 entrypoint.sh /usr/local/bin/entrypoint.sh

# Anonymous volume so inner-docker state never lands on the container's
# overlay; removed with the container (docker rm --volumes).
VOLUME /var/lib/docker

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]

# Slim variant: no Docker Engine. A job step that runs `docker` fails with
# command-not-found, which is the accurate error for a seccomp pool.
FROM base AS slim
COPY --chmod=0755 entrypoint-slim.sh /usr/local/bin/entrypoint.sh
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]

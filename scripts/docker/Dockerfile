# Runner image for the docker backend: Ubuntu 24.04 + Docker Engine
# (DinD, run inside gVisor) + actions-runner. Built locally by
# `github-qemu-runner refresh-image`; never pushed to a registry.
FROM ubuntu:24.04
ARG RUNNER_VERSION
ARG RUNNER_TARBALL_URL
ARG RUNNER_TARBALL_SHA256

RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates curl git && \
    install -m 0755 -d /etc/apt/keyrings && \
    curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc && \
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu noble stable" \
        > /etc/apt/sources.list.d/docker.list && \
    apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        docker-ce docker-ce-cli containerd.io docker-buildx-plugin && \
    rm -rf /var/lib/apt/lists/*

# uid 1001: ubuntu:24.04 ships an "ubuntu" user at 1000.
RUN useradd --create-home --uid 1001 runner && usermod -aG docker runner

RUN mkdir -p /opt/actions-runner && cd /opt/actions-runner && \
    curl -fsSL -o runner.tar.gz "$RUNNER_TARBALL_URL" && \
    { [ -z "$RUNNER_TARBALL_SHA256" ] || echo "$RUNNER_TARBALL_SHA256  runner.tar.gz" | sha256sum -c -; } && \
    tar xzf runner.tar.gz && rm runner.tar.gz && \
    apt-get update && ./bin/installdependencies.sh && rm -rf /var/lib/apt/lists/* && \
    chown -R runner:runner /opt/actions-runner

COPY entrypoint.sh /usr/local/bin/entrypoint.sh

# Anonymous volume so inner-docker state never lands on the container's
# overlay; removed with the container (docker rm --volumes).
VOLUME /var/lib/docker

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]

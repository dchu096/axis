# Axis DevTools sidecar — allowlisted tooling only.
# Build: docker build -t ghcr.io/dchu096/atlas-devtools:latest -f Dockerfile .
FROM alpine:3.20

RUN apk add --no-cache \
    bash \
    ca-certificates \
    curl \
    fastfetch \
    git \
    gzip \
    jq \
    less \
    nano \
    openssh-client \
    tar \
    unzip \
    vim \
    wget \
    && rm -rf /var/cache/apk/*

COPY motd /etc/motd
COPY profile.d/00-axis-devtools.sh /etc/profile.d/00-axis-devtools.sh
COPY fastfetch/config.jsonc /etc/fastfetch/config.jsonc

# Non-root friendly; Axis remaps to the configured pterodactyl/axis UID at runtime.
WORKDIR /home/container

# Keep a usable interactive shell; security comes from Axis mount flags + CapDrop.
ENV HOME=/home/container \
    TERM=xterm-256color

CMD ["/bin/bash", "-l"]

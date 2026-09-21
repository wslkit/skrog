# Assembles the Skrog rootfs.
#
# Built as a container image and then `docker export`ed, rather than unpacked
# and modified on the host. That matters: apk's post-install triggers run
# chrooted, and `apk --root` against a host directory fails them with exit 127
# (ca-certificates hashes, terminfo, iproute2), leaving a rootfs that looks
# populated but is subtly wrong. Building natively runs every trigger the way
# Alpine intends, and sidesteps the root-owned-files problem on the runner.
ARG ALPINE_TAG=3.24
# ALPINE_DIGEST pins the base by content (#88); the tag is kept alongside for
# readability but the digest is what Docker actually resolves. build.sh passes
# the value from versions.env; the default here is a same-value fallback for a
# bare `docker build`.
ARG ALPINE_DIGEST=sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b
FROM alpine:${ALPINE_TAG}@${ALPINE_DIGEST}

# The qemu-user emulator for the OTHER architecture (#462), so a foreign
# container can run under binfmt_misc. build.sh passes the package name from
# arch.sh: qemu-aarch64 in an amd64 rootfs, qemu-x86_64 in an arm64 one, and
# never one for the host's own architecture.
#
# Present but INERT. Nothing registers it here; emulation is opt-in per
# install, and shipping the binary is not the opt-in. An unregistered
# interpreter is 6.3 MB of file that never executes.
ARG QEMU_PKG
RUN test -n "$QEMU_PKG" || { echo "QEMU_PKG build-arg is required" >&2; exit 1; } \
    && apk add --no-cache "$QEMU_PKG"

# Runtime dependencies. None of these are in the Alpine minirootfs, and their
# absence only surfaces when the engine is actually booted:
#   iptables/ip6tables  dockerd cannot build container networking without them
#   iproute2            ip/tc, used for veth and bridge setup
#   ca-certificates     registry TLS
#   socat               the v0.1 pipe relay connects through it
#   e2fsprogs/xfsprogs  filesystem tooling for volumes
#   kmod                modprobe for overlay, br_netfilter, ip_tables
#   util-linux          mount, nsenter, unshare
#   pigz                parallel gunzip; a large, cheap win on image pulls
#   xz                  xz-compressed image layers
#   tini-static         becomes docker-init, which `docker run --init` needs.
#                       moby vendors tini rather than exposing a make target
#                       for it, so it comes from Alpine (also MIT-licensed).
#                       Copied rather than symlinked: an absolute symlink
#                       dangles whenever the rootfs is inspected from outside,
#                       which is exactly what the smoke test does.
RUN apk add --no-cache \
        socat \
        iptables ip6tables iproute2 \
        ca-certificates \
        e2fsprogs xfsprogs \
        kmod util-linux \
        pigz xz \
        tini-static \
    && cp /sbin/tini-static /usr/local/bin/docker-init

COPY bin/ /usr/local/bin/
RUN chmod 0755 /usr/local/bin/*

# Licence texts for everything shipped here (#205). Apache-2.0 section 4(a)
# requires giving recipients a copy, and every engine component is Apache-2.0;
# the Alpine userland is covered by the pointer in licenses/README.
COPY licenses/ /usr/share/licenses/

# Engine defaults: log rotation on from the first run (PLAN §05 v0.1).
COPY daemon.json /etc/docker/daemon.json

# WSL-side distro config. systemd is off: Skrog supervises dockerd itself, so
# an init system inside the distro buys nothing.
COPY wsl.conf /etc/wsl.conf

# What the rootfs declares about itself: `skrog install` reads engine-version
# when no version is given, and `commits` is the provenance record a security
# review reads.
COPY engine-version /etc/skrog/engine-version
COPY agent-version /etc/skrog/agent-version
COPY commits /etc/skrog/commits

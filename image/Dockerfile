# image/Dockerfile: the fence image `ccsbx-fence` (plan §3 U1a). It is what every fenced session
# runs: Debian slim by digest, Claude Code pinned by sha256 per architecture, git/rg/jq, the git shim
# (read verbs pass through; commit and push become requests in the outbox), an EMPTY /opt/ccf that the
# client layer fills later through the candidate channel, and uid 10001. It holds no credential:
# nothing is passed in but the pins (public values). The network is used at build time only: apt
# (from a dated snapshot.debian.org archive, so the packages are the same bytes every build) in both
# stages and the Claude Code download in the first, which is thrown away. The image keeps no setuid
# or setgid bit and has no `git` on PATH except the shim. Build it with image/build.sh (which
# supplies the pins from image/pins.env as build args; without them the build fails, because no pin
# lives in this file).
#
#   image/build.sh                      # ccsbx-fence:dev for this machine's architecture
ARG BASE

# ---- stage 1: fetch Claude Code and refuse anything that does not match the pinned sha256 --------
FROM ${BASE} AS fetch
ARG TARGETARCH
ARG CLAUDE_VERSION
ARG CLAUDE_URL_BASE
ARG CLAUDE_SHA256_AMD64
ARG CLAUDE_SHA256_ARM64
ARG SNAPSHOT
# Packages come from the dated snapshot archive (apt still verifies the signed Release files with
# the keyring in the base; the archive's Valid-Until has long passed, hence Check-Valid-Until=false).
RUN set -eu; test -n "$SNAPSHOT" || { echo "SNAPSHOT pin missing: build with image/build.sh" >&2; exit 1; }; \
    printf 'Types: deb\nURIs: http://snapshot.debian.org/archive/debian/%s/\nSuites: bookworm bookworm-updates\nComponents: main\nSigned-By: /usr/share/keyrings/debian-archive-keyring.gpg\n\nTypes: deb\nURIs: http://snapshot.debian.org/archive/debian-security/%s/\nSuites: bookworm-security\nComponents: main\nSigned-By: /usr/share/keyrings/debian-archive-keyring.gpg\n' "$SNAPSHOT" "$SNAPSHOT" > /etc/apt/sources.list.d/debian.sources; \
    apt-get -o Acquire::Check-Valid-Until=false update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*
RUN set -eu; \
    test -n "$CLAUDE_VERSION" -a -n "$CLAUDE_URL_BASE" || { echo "pins missing: build with image/build.sh" >&2; exit 1; }; \
    case "$TARGETARCH" in \
      amd64) plat=linux-x64;   sha="$CLAUDE_SHA256_AMD64" ;; \
      arm64) plat=linux-arm64; sha="$CLAUDE_SHA256_ARM64" ;; \
      *) echo "unsupported architecture '$TARGETARCH'" >&2; exit 1 ;; \
    esac; \
    test -n "$sha" || { echo "no sha256 pinned for $TARGETARCH" >&2; exit 1; }; \
    curl -fsSL --proto '=https' --tlsv1.2 -o /claude "$CLAUDE_URL_BASE/$CLAUDE_VERSION/$plat/claude"; \
    echo "$sha  /claude" | sha256sum -c -; \
    chmod 0755 /claude

# ---- stage 2: the image -------------------------------------------------------------------------
FROM ${BASE}
ARG SNAPSHOT
# Packages come from the dated snapshot archive (apt still verifies the signed Release files with
# the keyring in the base; the archive's Valid-Until has long passed, hence Check-Valid-Until=false).
# One RUN, so nothing is created and then deleted in a later layer (a deletion leaves the file in
# its layer): install, make the apt state go away, strip every setuid/setgid bit (chmod, not rm), move
# the real git out of PATH (the shim at /usr/local/bin/git is the only caller), add uid 10001.
RUN set -eu; test -n "$SNAPSHOT" || { echo "SNAPSHOT pin missing: build with image/build.sh" >&2; exit 1; }; \
    printf 'Types: deb\nURIs: http://snapshot.debian.org/archive/debian/%s/\nSuites: bookworm bookworm-updates\nComponents: main\nSigned-By: /usr/share/keyrings/debian-archive-keyring.gpg\n\nTypes: deb\nURIs: http://snapshot.debian.org/archive/debian-security/%s/\nSuites: bookworm-security\nComponents: main\nSigned-By: /usr/share/keyrings/debian-archive-keyring.gpg\n' "$SNAPSHOT" "$SNAPSHOT" > /etc/apt/sources.list.d/debian.sources; \
    apt-get -o Acquire::Check-Valid-Until=false update \
 && apt-get install -y --no-install-recommends ca-certificates git ripgrep jq \
 && rm -rf /var/lib/apt/lists/* \
 && find / -xdev -type f \( -perm -4000 -o -perm -2000 \) -exec chmod a-s {} + \
 && rm /usr/bin/git \
 && mkdir -p /opt/ccf /home/fence \
 && useradd --uid 10001 --user-group --home-dir /home/fence --no-create-home --shell /bin/sh fence \
 && chown 10001:10001 /home/fence
COPY --from=fetch /claude /usr/local/bin/claude
# The shim is the only `git` on PATH; the real binary is /usr/lib/git-core/git (git's own exec path).
COPY git-shim /usr/local/bin/git
RUN chmod 0755 /usr/local/bin/git
ENV PATH=/usr/local/bin:/usr/bin:/bin
ENV DISABLE_AUTOUPDATER=1
USER 10001:10001
WORKDIR /home/fence

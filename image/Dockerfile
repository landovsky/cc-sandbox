# image/Dockerfile: the fence image `ccsbx-fence` (plan §3 U1a). It is what every fenced session
# runs: Debian slim by digest, Claude Code pinned by sha256 per architecture, git/rg/jq, the git shim
# (read verbs pass through; commit and push become requests in the outbox), an EMPTY /opt/ccf that the
# client layer fills later through the candidate channel, and uid 10001. It holds no credential:
# nothing is passed in but the pins, and the only network use is the download in the first stage,
# which is thrown away. Build it with image/build.sh (which supplies the pins from image/pins.env as
# build args; without them the build fails, because no pin lives in this file).
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
RUN apt-get update \
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
    curl -fsSL -o /claude "$CLAUDE_URL_BASE/$CLAUDE_VERSION/$plat/claude"; \
    echo "$sha  /claude" | sha256sum -c -; \
    chmod 0755 /claude

# ---- stage 2: the image -------------------------------------------------------------------------
FROM ${BASE}
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git ripgrep jq \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /opt/ccf /home/fence \
 && useradd --uid 10001 --user-group --home-dir /home/fence --no-create-home --shell /bin/sh fence \
 && chown 10001:10001 /home/fence
COPY --from=fetch /claude /usr/local/bin/claude
# /usr/local/bin comes before /usr/bin in PATH, so `git` is the shim; the real one is /usr/bin/git.
COPY git-shim /usr/local/bin/git
RUN chmod 0755 /usr/local/bin/git
ENV DISABLE_AUTOUPDATER=1
USER 10001:10001
WORKDIR /home/fence

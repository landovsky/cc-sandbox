# image-side/Dockerfile — the sidecar image `ccsbx` (ghcr.io/landovsky/ccsbx): one static binary and nothing else.
# Why: a fenced pod's `side` container runs `ccsbx side serve` (the forwarder, the control socket) and takes the
# session's token over `kubectl exec ... ccsbx side load`. It holds the only credential in the pod, so it carries
# no shell, no package manager and no libc: whatever reaches it can run exactly one program.
#
# The binary is built OUTSIDE (the workflow runs `go build` for each architecture into bin/ccsbx-linux-<arch>), so
# the image has no toolchain layer and no base image to pin. `FROM scratch` is empty by definition.
# uid/gid 10002 is internal/k8s.SideUID: the pod spec runs it as that uid with a read-only root filesystem, and
# `side load` only accepts secrets from its own uid. The pod mounts /tmp and /run/ccsbx/* as volumes; the image
# needs no writable path of its own and has none (no /tmp here).
# Doors in the cluster are plain-HTTP ClusterIP addresses, so no CA bundle is shipped: a https door needs
# `--ca-file` (see `ccsbx side --help`) from a mounted volume.
# Build: image-side/build.sh (local, one architecture) or .github/workflows/side-image.yml (both, pushed by digest).
FROM scratch
ARG TARGETARCH
COPY --chmod=0555 bin/ccsbx-linux-${TARGETARCH} /usr/local/bin/ccsbx
ENV PATH=/usr/local/bin
USER 10002:10002
ENTRYPOINT ["/usr/local/bin/ccsbx", "side"]

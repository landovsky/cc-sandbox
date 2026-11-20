# image/ccf.Dockerfile: the CANDIDATE image = the fence image plus the client layer at /opt/ccf (plan §3 C3, H2). It exists so
# the layer reaches the fence the way plan H2 says, as a digest that can be tried under e2e-* profiles and promoted later,
# and not by baking it into the base image (which keeps /opt/ccf EMPTY, tests/image-test.sh checks that). The layer is a
# tar made by dotfiles ccf/ccf-pack (reproducible, files at the tar's root: ccf-hook, ccf-mcp, ...). The result is root-owned
# and read-only to the fence (uid 10001): the files cannot be edited. That protects the BYTES; what RUNS is protected by the
# managed settings (internal/settings: `/usr/bin/python3 -I`, pinned CCF_* env), not by this file. Build it with
# image/build-ccf.sh, which checks the tar's exact file list and digest, scans /opt/ccf for credentials and setid files, and
# checks what landed.
ARG BASE_IMAGE=ccsbx-fence:dev
FROM ${BASE_IMAGE}
ARG CCF_DIGEST
USER 0
# ADD of a local tar unpacks it; ownership and mode are then forced, whatever the tar said
ADD ccf.tar /opt/ccf/
RUN set -eu; test -n "$CCF_DIGEST"; \
    chown -R 0:0 /opt/ccf; \
    find /opt/ccf -type d -exec chmod 0555 {} +; \
    find /opt/ccf -type f -exec chmod a-s,go-w {} +; \
    chmod 0555 /opt/ccf/ccf-hook /opt/ccf/ccf-statusline /opt/ccf/ccf-mcp; \
    chmod 0444 /opt/ccf/ccfreq.py /opt/ccf/managed-settings.json /opt/ccf/managed-mcp.json; \
    printf '%s\n' "$CCF_DIGEST" > /opt/ccf/.digest; chmod 0444 /opt/ccf/.digest
LABEL org.ccsbx.ccf.digest="${CCF_DIGEST}"
USER 10001:10001

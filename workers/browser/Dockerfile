# Orka browser worker: Playwright's pinned Chromium behind Orka's /worker
# entrypoint. A container Task that sets this image and passes its command in
# args (leaving command empty) gets the managed worker flow: stdout becomes the
# result, /tmp/artifacts is uploaded, and a soft deadline delivers partial
# output. The Task is still treated as a custom image.
#
# The tags document human-readable versions; the manifest-list digests make
# every supported platform immutable. Keep PLAYWRIGHT_VERSION equal to
# workers/browser.PlaywrightVersion.

# The worker is cross-compiled on the build platform; only the final stage
# runs per target platform.
FROM --platform=$BUILDPLATFORM golang:1.27.2@sha256:5bc7f572bbaa98885a3a1fd9c0aa76b59e3e14e8628bfc316bbfd0c701e4818c AS builder
ARG TARGETOS=linux
ARG TARGETARCH

WORKDIR /workspace

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" go build -trimpath -ldflags='-s -w' -o /out/worker ./workers/general

FROM docker.io/library/node:24.22.1-trixie-slim@sha256:43b9ba1142b8dcf33fa8f922ca1be85714adc444e0586c7cf7726daaeabc1bea

ARG PLAYWRIGHT_VERSION=1.64.1

ENV PLAYWRIGHT_BROWSERS_PATH=/ms-playwright \
    NODE_PATH=/usr/local/lib/node_modules \
    NPM_CONFIG_AUDIT=false \
    NPM_CONFIG_FUND=false \
    NPM_CONFIG_UPDATE_NOTIFIER=false

# Playwright installs its own Chromium build and headless shell for this exact
# version, plus the system libraries they need. The fonts make screenshots of
# ordinary and emoji text legible. The node user already holds UID/GID 1000,
# which Orka forces on every Task Pod; it is renamed worker.
RUN set -eu; \
    npm install --global --no-audit --no-fund "playwright@${PLAYWRIGHT_VERSION}"; \
    test "$(playwright --version)" = "Version ${PLAYWRIGHT_VERSION}"; \
    apt-get update; \
    playwright install --with-deps chromium; \
    apt-get install -y --no-install-recommends ca-certificates git fonts-noto-core fonts-noto-color-emoji; \
    rm -rf /var/lib/apt/lists/* /root/.npm /tmp/*; \
    chmod -R a+rX "${PLAYWRIGHT_BROWSERS_PATH}"; \
    groupmod -n worker node; \
    usermod -l worker -d /tmp -s /bin/sh node; \
    printf '%s\n' \
      '#!/bin/sh' \
      '# Wait for the Pod network to settle. The IPv6 link-local address finishes' \
      '# duplicate-address detection about a second after the Pod starts, and' \
      '# Chromium treats that as a network change: requests in flight fail with' \
      '# net::ERR_NETWORK_CHANGED. A tentative address has flag 0x40 in' \
      '# /proc/net/if_inet6. Gives up after 5 s so a stuck address never blocks.' \
      '# As PID 1 the wrapper gets no default signal action, so a cancellation' \
      '# during the wait must be handled or the command would still start.' \
      'trap "exit 143" TERM INT' \
      'i=0' \
      'while [ "$i" -lt 100 ]; do' \
      '  tentative=0' \
      '  while read -r _addr _index _prefix _scope flags _name; do' \
      '    [ "$(( 0x$flags & 0x40 ))" -ne 0 ] && tentative=1' \
      '  done < /proc/net/if_inet6 2>/dev/null' \
      '  [ "$tentative" -eq 0 ] && exit 0' \
      '  i=$((i + 1)); sleep 0.05' \
      'done' \
      'echo "wait-for-network: an address stayed tentative for 5 s; continuing" >&2' \
      > /usr/local/bin/wait-for-network; \
    chmod 0755 /usr/local/bin/wait-for-network; \
    printf '%s\n' \
      '#!/bin/sh' \
      '# The image entrypoint: settle the network, then run the Task args under /worker.' \
      'trap "exit 143" TERM INT' \
      '/usr/local/bin/wait-for-network || exit $?' \
      'exec /worker "$@"' \
      > /usr/local/bin/browser-entrypoint; \
    chmod 0755 /usr/local/bin/browser-entrypoint; \
    printf '%s\n' \
      '#!/bin/sh' \
      '# Chromium for CLI use, with the flags Orka Task Pods need. This is the headless' \
      '# shell Playwright runs headless: unlike full Chromium, it has no sign-in, sync,' \
      '# or component-update services, so it makes no requests of its own.' \
      'set -eu' \
      'trap "exit 143" TERM INT' \
      '/usr/local/bin/wait-for-network' \
      'for chrome in /ms-playwright/chromium_headless_shell-*/chrome-headless-shell-linux*/chrome-headless-shell; do' \
      '  [ -x "$chrome" ] && exec "$chrome" --no-sandbox --disable-dev-shm-usage --disable-gpu \' \
      '    --user-data-dir=/tmp/chrome-profile "$@"' \
      'done' \
      'echo "chromium-headless: no Playwright headless shell under /ms-playwright" >&2' \
      'exit 1' \
      > /usr/local/bin/chromium-headless; \
    chmod 0755 /usr/local/bin/chromium-headless; \
    printf '%s\n' \
      '#!/bin/sh' \
      '# The playwright CLI. Chromium for this version is already in /ms-playwright, where' \
      '# `playwright install` waits on a lock on the read-only root filesystem until the' \
      '# Task deadline. So with that browser path, installing Chromium or its dependencies' \
      '# succeeds at once, silently so a JSON result stays intact, and other browsers are' \
      '# refused. Help, --list, --dry-run, and any other browser path go to the real CLI.' \
      'cli=/usr/local/lib/node_modules/playwright/cli.js' \
      'case "${1:-}" in' \
      '  install|install-deps)' \
      '    [ "${PLAYWRIGHT_BROWSERS_PATH:-}" = /ms-playwright ] || exec node "$cli" "$@"' \
      '    for arg in "$@"; do' \
      '      case "$arg" in --help|-h|--list|--dry-run) exec node "$cli" "$@" ;; esac' \
      '    done' \
      '    for arg in "$@"; do' \
      '      case "$arg" in' \
      '        install|install-deps|chromium|chromium-headless-shell|ffmpeg|-*) ;;' \
      '        *) echo "playwright $1: only Chromium is installed in this image, and browsers cannot be added at run time" >&2; exit 1 ;;' \
      '      esac' \
      '    done' \
      '    exit 0 ;;' \
      'esac' \
      'exec node "$cli" "$@"' \
      > /usr/local/bin/playwright.orka; \
    chmod 0755 /usr/local/bin/playwright.orka; \
    mv -f /usr/local/bin/playwright.orka /usr/local/bin/playwright; \
    test "$(playwright --version)" = "Version ${PLAYWRIGHT_VERSION}"

COPY LICENSE NOTICE.md /usr/share/licenses/orka/
COPY --from=builder --chown=0:0 /out/worker /worker

LABEL org.opencontainers.image.title="Orka browser worker" \
      org.opencontainers.image.description="Playwright Chromium behind the Orka worker entrypoint for browser container Tasks" \
      org.opencontainers.image.licenses="MIT" \
      ai.orka.worker.kind="browser" \
      ai.orka.playwright.version="${PLAYWRIGHT_VERSION}"

# Orka forces UID/GID 1000 and a read-only root filesystem; only /tmp (and
# /workspace and /home/worker with a workspace) is writable.
ENV HOME=/tmp \
    XDG_CACHE_HOME=/tmp/.cache \
    XDG_CONFIG_HOME=/tmp/.config \
    NPM_CONFIG_CACHE=/tmp/npm-cache
WORKDIR /tmp
USER 1000:1000

# browser-entrypoint execs /worker once the Pod network has settled.
ENTRYPOINT ["/usr/local/bin/browser-entrypoint"]

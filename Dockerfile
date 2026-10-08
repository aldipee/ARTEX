# syntax=docker/dockerfile:1
#
# Runtime image (no compilation inside the image): install common tools and drop in the **pre-built Linux single binary**.
# The binary is cross-compiled by CI's binaries job (pure Go, no QEMU), placed by target arch under
# dist/<TARGETARCH>/artex in the build context. This way, for multi-arch builds arm64 only needs to emulate the apt layer,
# no longer emulating Next/Go compilation, so builds are much faster.
#
# For local manual image builds, prepare the binary yourself first:
#   cd web && npm run build:static && cd ..
#   cp -r web/out server/webui/dist
#   CGO_ENABLED=0 GOARCH=amd64 go build -tags embedui -o dist/amd64/artex ./cmd/artex
#   docker build -t artex:local .
FROM python:3.12-slim-bookworm
ARG TARGETARCH
# Common tools: ripgrep / curl / vim, plus a set of standard recon utilities (add/remove as needed).
# Node is installed from NodeSource at 20.x: bookworm's bundled apt nodejs is 18, while Playwright requires >=20.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ripgrep curl wget vim git jq unzip \
      dnsutils iputils-ping netcat-openbsd inetutils-telnet whois nmap \
    && curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*
# Preinstall Playwright MCP and CLI (globally), so runtime no longer needs npx to download them.
# @playwright/mcp: browser MCP runs directly via `npx @playwright/mcp` (already installed globally, no -y/@latest needed).
# @playwright/cli: provides playwright-cli; verify it works with --help after install.
# Then install playwright (provides browser management); after that, preinstall chromium and its system deps with --with-deps,
# so MCP/CLI are usable on first start inside the container without downloading browsers.
RUN npm install -g @playwright/mcp@latest @playwright/cli@latest playwright@latest \
    && playwright-cli --help \
    && playwright install --with-deps chromium \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
# Pre-built binary for the matching arch (dist/amd64/artex or dist/arm64/artex)
COPY dist/${TARGETARCH}/artex /app/artex
# Daemon start script: decides whether to relaunch after the process exits based on its exit code; the page's one-click update relies on it to swap in the new build.
# It also forwards SIGTERM to artex — docker stop only sends the signal to PID 1,
# and without forwarding artex never receives it and can't shut down gracefully, getting hard-killed by SIGKILL after 10 seconds.
COPY start.sh /app/start.sh
RUN chmod +x /app/artex /app/start.sh
COPY skills/ /app/skills/
# data/ (SQLite + jwt.key) persistence point
VOLUME ["/app/data"]
EXPOSE 8787 8788
ENTRYPOINT ["/app/start.sh"]
CMD ["-addr", ":8787", "-proxy", ":8788"]

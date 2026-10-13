# syntax=docker/dockerfile:1
#
# Runtime image (no compilation inside the image): install only the common
# tools and drop in the **pre-built Linux single binary**.
# The CI `binaries` job cross-compiles the binary (pure Go, no QEMU) and
# places it at `dist/<TARGETARCH>/artex` in the build context. This way
# arm64 multi-arch builds only emulate the apt layer and skip emulating
# the Next/Go toolchain, which is dramatically faster.
#
# To build the image locally, prepare the binary yourself first:
#   cd web && npm run build:static && cd ..
#   cp -r web/out server/webui/dist
#   CGO_ENABLED=0 GOARCH=amd64 go build -tags embedui -o dist/amd64/artex ./cmd/artex
#   docker build -t artex:local .
FROM python:3.12-slim-bookworm
ARG TARGETARCH
# Common tools: ripgrep / curl / vim, plus a recon starter pack (add/remove as needed).
# Node is installed from NodeSource at 20.x: bookworm's apt nodejs is 18, Playwright requires >=20.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ripgrep curl wget vim git jq unzip \
      dnsutils iputils-ping netcat-openbsd inetutils-telnet whois nmap \
    && curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*
# Pre-install Playwright MCP and CLI (global) so the runtime never has to npx download.
# @playwright/mcp: browser MCP is `npx @playwright/mcp` (already globally installed, no -y/@latest).
# @playwright/cli: provides playwright-cli; --help after install verifies the executable works.
# Then install playwright (browser manager) with --with-deps to provision chromium and its
# system dependencies, so the container's MCP/CLI is ready on first launch without downloading.
RUN npm install -g @playwright/mcp@latest @playwright/cli@latest playwright@latest \
    && playwright-cli --help \
    && playwright install --with-deps chromium \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
# Pre-built architecture-matched binary (dist/amd64/artex or dist/arm64/artex)
COPY dist/${TARGETARCH}/artex /app/artex
# Daemon startup script: after the process exits, decide whether to relaunch based on
# the exit code; the in-page one-click update relies on it to swap binaries. It is
# also responsible for forwarding SIGTERM to artex — `docker stop` only signals PID 1,
# so without forwarding artex never receives it and is hard-killed by SIGKILL after 10 s.
COPY start.sh /app/start.sh
RUN chmod +x /app/artex /app/start.sh
COPY skills/ /app/skills/
# data/ (SQLite + jwt.key) persistence mount point
VOLUME ["/app/data"]
EXPOSE 8787 8788
ENTRYPOINT ["/app/start.sh"]
CMD ["-addr", ":8787", "-proxy", ":8788"]

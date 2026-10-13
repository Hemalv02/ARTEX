# syntax=docker/dockerfile:1
#
# Runtime image (no compilation inside the image): install only common tools and drop in a
# **prebuilt Linux single binary**.
# The binary is cross-compiled by CI's binaries job (pure Go, no QEMU) and placed per target
# architecture at dist/<TARGETARCH>/artex in the build context. This way, for multi-arch builds
# arm64 only has to emulate the apt layer and no longer emulates the Next/Go compilation, which
# is much faster.
#
# When building the image manually in your local environment, prepare the binary yourself first:
#   cd web && npm run build:static && cd ..
#   cp -r web/out server/webui/dist
#   CGO_ENABLED=0 GOARCH=amd64 go build -tags embedui -o dist/amd64/artex ./cmd/artex
#   docker build -t artex:local .
FROM python:3.12-slim-bookworm
ARG TARGETARCH
# Common tools: ripgrep / curl / vim, plus a set of recon staples (add or remove as needed).
# Node is installed from NodeSource at 20.x: bookworm's bundled apt nodejs is 18, but Playwright requires >=20.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ripgrep curl wget vim git jq unzip \
      dnsutils iputils-ping netcat-openbsd inetutils-telnet whois nmap \
    && curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*
# Preinstall Playwright MCP and CLI (globally) so there's no npx network download at runtime.
# @playwright/mcp: the browser MCP runs directly via `npx @playwright/mcp` (already installed globally, no -y/@latest needed).
# @playwright/cli: provides playwright-cli; run --help after install to verify it's executable.
# Then install playwright (provides browser management) and use --with-deps to pre-provision chromium and its system dependencies,
# so the MCP/CLI inside the container works on first startup without downloading the browser over the network.
RUN npm install -g @playwright/mcp@latest @playwright/cli@latest playwright@latest \
    && playwright-cli --help \
    && playwright install --with-deps chromium \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
# The prebuilt binary for the matching architecture (dist/amd64/artex or dist/arm64/artex)
COPY dist/${TARGETARCH}/artex /app/artex
# Supervisor startup script: after the process exits, its exit code decides whether to relaunch it;
# the in-page one-click update relies on it to swap in the new binary.
# It also forwards SIGTERM to artex -- docker stop only sends the signal to PID 1,
# and without forwarding, artex never receives it, can't shut down gracefully, and is hard-killed by SIGKILL after 10 seconds.
COPY start.sh /app/start.sh
RUN chmod +x /app/artex /app/start.sh
COPY skills/ /app/skills/
# data/ (SQLite + jwt.key) persistence point
VOLUME ["/app/data"]
EXPOSE 8787 8788
ENTRYPOINT ["/app/start.sh"]
CMD ["-addr", ":8787", "-proxy", ":8788"]

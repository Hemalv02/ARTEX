#!/usr/bin/env bash
# ARTEX update script: 1) Docker update (pull new image, recreate)  2) local build update (rebuild binary)
# Pairs with install.sh: install handles first-time setup, update handles upgrades to new versions.
# No manual DB migration needed -- artex idempotently re-runs schema.sql on every startup
# (including ADD COLUMN / CREATE INDEX IF NOT EXISTS), so "restart = migrate". Data
# (pgdata volume, ./data, ./skills) is not affected.
set -euo pipefail
cd "$(cd "$(dirname "$0")" && pwd)"

info(){ printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok(){   printf '\033[32m[+]\033[0m %s\n' "$*"; }
warn(){ printf '\033[33m[!]\033[0m %s\n' "$*"; }
die(){  printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }
ask(){  local p="$1" d="${2:-}" a; read -rp "$p${d:+ [$d]}: " a; echo "${a:-$d}"; }

# ── Optional: sync repo to latest code (updates compose/scripts/local source) ───────
sync_repo(){
  [ -d .git ] && command -v git >/dev/null 2>&1 || { warn "Not a git working copy, skipping git pull"; return; }
  [ "$(ask 'Pull latest code (git pull --ff-only)? (y/n)' y)" = y ] || return
  if ! git pull --ff-only; then
    warn "git pull could not fast-forward (local changes or branch divergence) -- please resolve manually and retry; using current code this time"
  fi
}

# ── 1) Docker update ───────────────────────────────
update_docker(){
  command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1 \
    || die "docker / docker compose not detected; please run ./install.sh first to deploy"
  [ -f .env ] || die ".env not found; please run ./install.sh first for initial deployment"

  # Optional: upgrade to a specific image tag (blank keeps ARTEX_TAG in .env, default latest)
  local tag; tag="$(ask 'Target image tag (Enter to keep .env / latest)' '')"
  if [ -n "$tag" ]; then
    if grep -q '^ARTEX_TAG=' .env; then
      sed -i.bak "s|^ARTEX_TAG=.*|ARTEX_TAG=${tag}|" .env && rm -f .env.bak
    else
      printf '\nARTEX_TAG=%s\n' "$tag" >> .env
    fi
    ok "ARTEX_TAG set to ${tag}"
  fi

  # Only touch artex: postgres is pinned to 16-alpine, no need to upgrade it (pulling wastes
  # bandwidth, and a major-version bump can introduce compatibility risks). artex declares
  # depends_on postgres, so `up` with the service name will start pg if it isn't running,
  # and keep it as-is (no recreate) when it's already up.
  info "Pulling new image (artex only)..."
  docker compose pull artex
  info "Rebuilding and starting (schema auto-migrates when artex restarts)..."
  docker compose up -d artex
  ok "Update complete -> http://localhost:8787"
  info "View logs: docker compose logs -f artex"
  info "Clean up old images (optional): docker image prune -f"
}

# ── 2) Local build update ──────────────────────────────
update_local(){
  command -v go >/dev/null 2>&1 || die "Go (>=1.26) not detected: https://go.dev/dl/"
  [ -f config.json ] || warn "config.json not found -- if this is a first-time deployment, please use ./install.sh"
  ok "Go: $(go version)"

  if command -v npm >/dev/null 2>&1; then
    info "Rebuilding frontend static assets..."
    ( cd web && npm ci && npm run build:static )
    rm -rf server/webui/dist && cp -r web/out server/webui/dist
    info "Recompiling single embedded binary..."
    CGO_ENABLED=0 go build -tags embedui -trimpath -o artex ./cmd/artex
  else
    warn "npm not detected: compiling backend **without embedded frontend** (run npm run dev separately for the frontend)"
    CGO_ENABLED=0 go build -o artex ./cmd/artex
  fi
  ok "Build complete -> ./artex"
  warn "Please restart the running artex process for changes to take effect (schema auto-migrates on restart)"
}

echo "=============================="
echo "  ARTEX Update"
echo "  1) Docker update (pull new image, recreate)"
echo "  2) Local update (rebuild with go)"
echo "=============================="
case "$(ask 'Choice' 1)" in
  1) sync_repo; update_docker ;;
  2) sync_repo; update_local ;;
  *) die "Invalid choice" ;;
esac

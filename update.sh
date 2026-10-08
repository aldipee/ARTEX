#!/usr/bin/env bash
# ARTEX update script: ① Docker update (pull new image and rebuild)  ② local compile update (rebuild binary)
# Complements install.sh: install does the first deployment, update upgrades to a new version.
# DB migrations need no manual step — artex idempotently reruns schema.sql on every startup (including ADD COLUMN/CREATE
# INDEX IF NOT EXISTS), so "restart = migrate". Data (the pgdata volume, ./data, ./skills) is unaffected.
set -euo pipefail
cd "$(cd "$(dirname "$0")" && pwd)"

info(){ printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok(){   printf '\033[32m[+]\033[0m %s\n' "$*"; }
warn(){ printf '\033[33m[!]\033[0m %s\n' "$*"; }
die(){  printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }
ask(){  local p="$1" d="${2:-}" a; read -rp "$p${d:+ [$d]}: " a; echo "${a:-$d}"; }

# ── optional: sync the repo to the latest code (compose/scripts/local-compile sources rely on it) ───────
sync_repo(){
  [ -d .git ] && command -v git >/dev/null 2>&1 || { warn "not a git working copy, skipping git pull"; return; }
  [ "$(ask 'Pull latest code (git pull --ff-only)? (y/n)' y)" = y ] || return
  if ! git pull --ff-only; then
    warn "git pull could not fast-forward (local changes or branch diverged) — resolve manually and retry; using current code this time"
  fi
}

# ── ① Docker update ───────────────────────────────
update_docker(){
  command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1 \
    || die "docker / docker compose not found, please deploy with ./install.sh first"
  [ -f .env ] || die ".env not found, please run ./install.sh to complete the first deployment"

  # optional: upgrade to a specific version tag (leave blank to keep ARTEX_TAG from .env, defaulting to latest)
  local tag; tag="$(ask 'Target image tag (Enter to keep .env / latest)' '')"
  if [ -n "$tag" ]; then
    if grep -q '^ARTEX_TAG=' .env; then
      sed -i.bak "s|^ARTEX_TAG=.*|ARTEX_TAG=${tag}|" .env && rm -f .env.bak
    else
      printf '\nARTEX_TAG=%s\n' "$tag" >> .env
    fi
    ok "ARTEX_TAG set to ${tag}"
  fi

  # Only touch artex: postgres is pinned to 16-alpine and doesn't need upgrading (pulling it wastes bandwidth,
  # and a major-version change brings compatibility risk). artex declares depends_on postgres, so bringing up the service by name
  # auto-starts pg if it isn't running, and leaves an already-running one untouched, without rebuilding.
  info "pulling new image (artex only)…"
  docker compose pull artex
  info "rebuilding and starting (schema auto-migrates on artex restart)…"
  docker compose up -d artex
  ok "update complete → http://localhost:8787"
  info "view logs: docker compose logs -f artex"
  info "clean up old images (optional): docker image prune -f"
}

# ── ② local compile update ──────────────────────────────
update_local(){
  command -v go >/dev/null 2>&1 || die "Go not found (>=1.26): https://go.dev/dl/"
  [ -f config.json ] || warn "config.json not found — for first deployment use ./install.sh instead"
  ok "Go: $(go version)"

  if command -v npm >/dev/null 2>&1; then
    info "rebuilding frontend static assets…"
    ( cd web && npm ci && npm run build:static )
    rm -rf server/webui/dist && cp -r web/out server/webui/dist
    info "recompiling embedded single binary…"
    CGO_ENABLED=0 go build -tags embedui -trimpath -o artex ./cmd/artex
  else
    warn "npm not found: compiling a backend without an embedded frontend (run npm run dev separately for the frontend)"
    CGO_ENABLED=0 go build -o artex ./cmd/artex
  fi
  ok "compiled → ./artex"
  warn "please restart the running artex process to take effect (schema auto-migrates on restart)"
}

echo "=============================="
echo "  ARTEX update"
echo "  1) Docker update (pull new image and rebuild)"
echo "  2) local update (recompile with go)"
echo "=============================="
case "$(ask 'Choice' 1)" in
  1) sync_repo; update_docker ;;
  2) sync_repo; update_local ;;
  *) die "invalid choice" ;;
esac

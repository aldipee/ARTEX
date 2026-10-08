#!/usr/bin/env bash
# ARTEX install script: ① all-Docker  ② local compile and run
set -euo pipefail
cd "$(cd "$(dirname "$0")" && pwd)"

info(){ printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok(){   printf '\033[32m[+]\033[0m %s\n' "$*"; }
warn(){ printf '\033[33m[!]\033[0m %s\n' "$*"; }
die(){  printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }
ask(){  local p="$1" d="${2:-}" a; read -rp "$p${d:+ [$d]}: " a; echo "${a:-$d}"; }
rand(){ head -c 18 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 24; }

# ── docker environment detection / auto install ───────────────────
ensure_docker(){
  if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
    ok "docker and docker compose detected"; return
  fi
  warn "docker / docker compose not found"
  case "$(uname -s)" in
    Linux)
      if [ "$(ask 'Auto-install Docker? (y/n)' y)" = y ]; then
        curl -fsSL https://get.docker.com | sh
        sudo usermod -aG docker "$USER" || true
        ok "Docker installed (re-login required after group change for passwordless sudo)"
      else
        die "please install docker yourself and retry"
      fi ;;
    Darwin) die "on macOS please install Docker Desktop: https://www.docker.com/products/docker-desktop/" ;;
    *)      die "please install docker yourself and retry" ;;
  esac
}

# ── ① all-Docker ───────────────────────────────
install_docker(){
  ensure_docker
  if [ ! -f .env ]; then
    cp .env.example .env 2>/dev/null || true
    local pw key
    pw="$(ask 'Postgres password (Enter to generate randomly)' "$(rand)")"
    key="$(ask 'ANTHROPIC_API_KEY (may leave blank, configure in UI later)' '')"
    sed -i.bak "s|^POSTGRES_PASSWORD=.*|POSTGRES_PASSWORD=${pw}|" .env
    sed -i.bak "s|^ANTHROPIC_API_KEY=.*|ANTHROPIC_API_KEY=${key}|" .env
    rm -f .env.bak
    ok ".env generated (POSTGRES_PASSWORD set)"
  else
    info "reusing existing .env"
  fi
  info "pulling images and starting…"
  docker compose pull || true
  docker compose up -d
  ok "started → http://localhost:8787"
  info "view logs: docker compose logs -f artex"
}

# ── ② local compile and run ──────────────────────────────
install_local(){
  echo "Database setup method:"
  echo "  1) connect to an existing PostgreSQL"
  echo "  2) start a PostgreSQL with Docker (requires docker)"
  case "$(ask 'Choice' 1)" in
    2)
      ensure_docker
      local pw; pw="$(ask 'Postgres password (Enter for random)' "$(rand)")"
      docker run -d --name artex-pg -p 5432:5432 \
        -e POSTGRES_USER=artex -e POSTGRES_PASSWORD="$pw" -e POSTGRES_DB=artex \
        -v artex-pg:/var/lib/postgresql/data postgres:16-alpine
      DB_HOST=127.0.0.1 DB_PORT=5432 DB_USER=artex DB_PASS="$pw" DB_NAME=artex DB_SSL=disable ;;
    *)
      DB_HOST="$(ask 'Database host' 127.0.0.1)"
      DB_PORT="$(ask 'Port' 5432)"
      DB_USER="$(ask 'User' artex)"
      DB_PASS="$(ask 'Password' '')"
      DB_NAME="$(ask 'Database name' artex)"
      DB_SSL="$(ask 'sslmode (disable/require)' disable)" ;;
  esac

  # generate config.json
  cat > config.json <<JSON
{
  "database": {
    "host": "${DB_HOST}",
    "port": ${DB_PORT},
    "user": "${DB_USER}",
    "password": "${DB_PASS}",
    "dbname": "${DB_NAME}",
    "sslmode": "${DB_SSL}"
  }
}
JSON
  ok "config.json generated"

  # go environment check
  command -v go >/dev/null 2>&1 || die "Go not found, please install Go (>=1.26) first: https://go.dev/dl/"
  ok "Go: $(go version)"

  # embedding the frontend requires node to produce static assets
  if command -v npm >/dev/null 2>&1; then
    info "building frontend static assets…"
    ( cd web && npm ci && npm run build:static )
    rm -rf server/webui/dist && cp -r web/out server/webui/dist
    info "compiling embedded single binary…"
    CGO_ENABLED=0 go build -tags embedui -trimpath -o artex ./cmd/artex
  else
    warn "npm not found: will compile a backend without an embedded frontend (run npm run dev separately for the frontend)"
    CGO_ENABLED=0 go build -o artex ./cmd/artex
  fi
  ok "compiled → ./artex"

  info "starting… (Ctrl-C to exit)"
  ./artex
}

echo "=============================="
echo "  ARTEX install"
echo "  1) all-Docker install"
echo "  2) run locally (go compile)"
echo "=============================="
case "$(ask 'Choice' 1)" in
  1) install_docker ;;
  2) install_local ;;
  *) die "invalid choice" ;;
esac

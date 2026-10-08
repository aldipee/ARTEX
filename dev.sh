#!/usr/bin/env bash
# Dev mode: backend (:8787) + traffic proxy (:8788) and frontend next dev (:5173) run together.
# The frontend /api proxies to the backend; Ctrl-C exits everything.
#
# For the single-binary (embedded frontend) approach see the "single binary" section in README; this script is not for that.
set -euo pipefail
cd "$(dirname "$0")"

# On exit, kill all child processes in this process group (backend + frontend).
cleanup() { kill 0 2>/dev/null || true; }
trap cleanup EXIT INT TERM

# Backend (plain go run, no embedded frontend); the concurrent work agent count is configured in "system settings".
go run ./cmd/artex -addr :8787 -proxy 127.0.0.1:8788 &

# Frontend hot reload (Vite/Next dev server, /api proxied to :8787).
( cd web && npm run dev ) &

echo "[dev] backend :8787 / proxy :8788 / frontend http://localhost:5173  (Ctrl-C to exit)"
wait

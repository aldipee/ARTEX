#!/bin/sh
# ARTEX daemon start script (Linux / macOS / Docker ENTRYPOINT)
#
# Usage:
#   ./start.sh                       run in foreground (Ctrl-C to stop)
#   nohup ./start.sh >artex.log 2>&1 &   run in background
#   ./start.sh -addr :9000           extra args passed through to artex as-is
#
# It does one thing: run artex, then decide by its exit code whether to relaunch after it exits.
#
#   0      user stopped normally → exit loop
#   75     program requested restart → rerun immediately (user clicked "one-click update" or "rollback" on the page)
#   other  crash                → rerun with backoff (1→2→4…up to 60 seconds)
#
# Deliberately no download, SHA256 verification, or binary swap here: that logic would have to be written twice (sh and bat),
# and it's exactly the part that must not go wrong — once a broken binary is swapped in, this script would faithfully
# relaunch it over and over, and the user could only recover by hand on the machine. So verification/swap stay in Go (selfupdate package),
# done by artex itself at startup, keeping the script dumb.
set -u

cd "$(dirname "$0")" || exit 1

BIN=./artex
[ -x "$BIN" ] || { echo "[artex] executable not found: $BIN" >&2; exit 1; }

RESTART_CODE=75
MAX_DELAY=60

child=0
stopping=0

# Forward stop signals to the artex binary itself.
#
# Required under Docker: docker stop sends SIGTERM only to PID 1 (i.e. this script),
# not to child processes. Without forwarding, artex never gets the signal and can't shut down gracefully, getting SIGKILL'd after 10 seconds
# and aborting any running tasks mid-flight.
forward() {
	stopping=1
	if [ "$child" -ne 0 ]; then
		kill -TERM "$child" 2>/dev/null || true
	fi
}
trap forward INT TERM

delay=1
while :; do
	"$BIN" "$@" &
	child=$!

	# A signal interrupts wait and makes it return >128. The child is actually still shutting down gracefully,
	# so we must wait once more to get its real exit code.
	wait "$child"
	code=$?
	if [ "$code" -gt 128 ]; then
		wait "$child"
		code=$?
	fi
	child=0

	if [ "$stopping" -eq 1 ]; then
		echo "[artex] stopped"
		exit 0
	fi

	case "$code" in
		0)
			echo "[artex] exited normally"
			exit 0
			;;
		"$RESTART_CODE")
			# Update/rollback ready: after relaunch, artex completes the swap at startup (see selfupdate.Bootstrap).
			echo "[artex] restart requested (applying new version)…"
			delay=1
			;;
		*)
			echo "[artex] abnormal exit (code=$code), restarting in ${delay}s" >&2
			sleep "$delay"
			delay=$((delay * 2))
			[ "$delay" -gt "$MAX_DELAY" ] && delay=$MAX_DELAY
			;;
	esac
done

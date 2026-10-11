#!/bin/sh
# ARTEX daemon startup script (Linux / macOS / Docker ENTRYPOINT)
#
# Usage:
#   ./start.sh                       Run in the foreground (Ctrl-C to stop)
#   nohup ./start.sh >artex.log 2>&1 &   Run in the background as a daemon
#   ./start.sh -addr :9000           Extra args are passed through to artex verbatim
#
# It only does one thing: runs artex, and after the process exits, decides
# whether to relaunch it based on the exit code.
#
#   0      User-initiated normal stop   -> Exit the loop
#   75     Program-requested restart    -> Rerun immediately ("One-click update"
#                                           or "Rollback" was clicked in the UI)
#   other  Crash                        -> Rerun after backoff (1->2->4... up to 60s)
#
# Intentionally NOT doing downloads, SHA256 verification, or binary swaps
# here: that logic would have to be written twice (sh + bat), and it's
# exactly the part that cannot be wrong — if a non-runnable binary is
# swapped in, this script will faithfully keep relaunching it and the only
# recourse is to SSH in and fix it by hand. So verification/swap is fully
# delegated to Go (the selfupdate package); artex performs it itself at
# startup, and the script stays foolproof.
set -u

cd "$(dirname "$0")" || exit 1

BIN=./artex
[ -x "$BIN" ] || { echo "[artex] Executable not found: $BIN" >&2; exit 1; }

RESTART_CODE=75
MAX_DELAY=60

child=0
stopping=0

# Forward stop signals to the artex process itself.
#
# This is required under Docker: `docker stop` only sends SIGTERM to PID 1
# (i.e. this script), not to children. Without forwarding, artex never sees
# the signal, can't shut down gracefully, and after 10s is hard-killed by
# SIGKILL — tasks in flight get cut off mid-run.
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

	# A signal interrupts `wait` and makes it return >128. At that point the
	# child is still in the middle of a graceful shutdown, so we must wait
	# a second time to get its real exit code.
	wait "$child"
	code=$?
	if [ "$code" -gt 128 ]; then
		wait "$child"
		code=$?
	fi
	child=0

	if [ "$stopping" -eq 1 ]; then
		echo "[artex] Stopped"
		exit 0
	fi

	case "$code" in
		0)
			echo "[artex] Normal exit"
			exit 0
			;;
		"$RESTART_CODE")
			# Update/rollback is staged: after relaunch, artex will perform
			# the binary swap at startup (see selfupdate.Bootstrap).
			echo "[artex] Restart requested (applying new version)..."
			delay=1
			;;
		*)
			echo "[artex] Abnormal exit (code=$code), restarting in ${delay}s" >&2
			sleep "$delay"
			delay=$((delay * 2))
			[ "$delay" -gt "$MAX_DELAY" ] && delay=$MAX_DELAY
			;;
	esac
done

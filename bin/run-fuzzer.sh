#!/bin/sh
# Overnight soak runner: time-budgeted outer loop around luafuzz chunks.
#
# luafuzz self-restarts every 2000 cases via exec (process-bounded chunks —
# wasmtime per-Store resources are only reliably reclaimed at process exit),
# so this loop only regains control when a chunk WEDGES (exit 99 via its
# wall guard) or the budget is spent. Exit 0 = the -max budget is spent.
#
# Battery gate: the fuzz load can draw more power than the adapter supplies,
# draining the battery even on AC. A watchdog polls the battery while the
# fuzzer runs; below BATTERY_PAUSE it stops luafuzz gracefully (the soak
# state resumes) and idles until the battery recharges to BATTERY_RESUME
# (hysteresis so it doesn't flap at the boundary). BATTERY_PAUSE=0 disables.
#
# usage: bin/run-fuzzer.sh [extra luafuzz flags]   (default budget 390m)
BUDGET="${FUZZ_BUDGET:-390m}"
BATTERY_PAUSE="${BATTERY_PAUSE:-50}"
BATTERY_RESUME="${BATTERY_RESUME:-60}"
BATTERY_POLL="${BATTERY_POLL:-30}"
BATTERY_CHECK="${BATTERY_CHECK:-$HOME/go/bin/battery-check}"

case "$BATTERY_PAUSE$BATTERY_RESUME$BATTERY_POLL" in
  ''|*[!0-9]*) echo "run-fuzzer: BATTERY_* values must be integers" >&2; exit 2 ;;
esac
[ "$BATTERY_RESUME" -lt "$BATTERY_PAUSE" ] && BATTERY_RESUME=$((BATTERY_PAUSE + 5))

cd "$(dirname "$0")/.." || exit 2

if [ "$BATTERY_PAUSE" -gt 0 ] && [ ! -x "$BATTERY_CHECK" ]; then
  echo "run-fuzzer: warning: $BATTERY_CHECK not usable — battery gate disabled" >&2
  BATTERY_PAUSE=0
fi

# Build once so battery restarts exec the binary directly; this also hands
# the watchdog the real luafuzz pid, which is stable across the per-chunk
# self-execs.
BIN="$(mktemp "${TMPDIR:-/tmp}/luafuzz.XXXXXX")" || exit 2
trap 'rm -f "$BIN"' EXIT
trap 'rm -f "$BIN"; exit 143' TERM INT
go build -o "$BIN" ./cmd/luafuzz || exit 2

# battery_ok LEVEL — success when the battery is at or above LEVEL. A
# battery-check FAILURE (exit 2) also succeeds: never park the soak on a
# tooling hiccup, only on a genuinely low battery.
battery_ok() {
  "$BATTERY_CHECK" --level "$1" >/dev/null 2>&1
  rc=$?
  [ "$rc" -eq 2 ] && echo "run-fuzzer: battery status unavailable — continuing" >&2
  [ "$rc" -ne 1 ]
}

# run_under_watchdog — one luafuzz invocation with the battery watchdog.
# Returns the luafuzz exit code, or 1 when the watchdog stopped it for a
# recharge (the outer loop then restarts it; soak state resumes).
run_under_watchdog() {
  "$BIN" -soak -max "$BUDGET" "$@" &
  fz=$!
  while kill -0 "$fz" 2>/dev/null; do
    if ! battery_ok "$BATTERY_PAUSE"; then
      echo "run-fuzzer: battery below $BATTERY_PAUSE% — stopping fuzzer for recharge" >&2
      kill -TERM "$fz" 2>/dev/null
      wait "$fz"
      until battery_ok "$BATTERY_RESUME"; do sleep "$BATTERY_POLL"; done
      echo "run-fuzzer: battery recharged (≥ $BATTERY_RESUME%) — resuming soak" >&2
      return 1
    fi
    sleep "$BATTERY_POLL"
  done
  wait "$fz"
}

while true; do
  if [ "$BATTERY_PAUSE" -gt 0 ]; then
    until battery_ok "$BATTERY_PAUSE"; do sleep "$BATTERY_POLL"; done
  fi
  run_under_watchdog "$@"
  rc=$?
  [ "$rc" -eq 0 ] && break          # budget spent / clean end
  [ "$rc" -ge 2 ] && exit "$rc"     # usage / lock error — stop
  echo "run-fuzzer: chunk exited $rc — restarting (state resumes)" >&2
  sleep 5
done

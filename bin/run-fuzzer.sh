#!/bin/sh
# Overnight soak runner: time-budgeted outer loop around luafuzz chunks.
#
# luafuzz self-restarts every 2000 cases (process-bounded chunks —
# wasmtime per-Store resources are only reliably reclaimed at process
# exit), but a WEDGED chunk exits 99 via its wall guard and needs this
# outer loop to restart it. Exit 0 = the -max budget is spent.
#
# usage: bin/run-fuzzer.sh [extra luafuzz flags]   (default budget 390m)
BUDGET="${FUZZ_BUDGET:-390m}"
cd "$(dirname "$0")/.." || exit 2
while true; do
  go run ./cmd/luafuzz -soak -max "$BUDGET" "$@"
  rc=$?
  [ "$rc" -eq 0 ] && break          # budget spent / clean end
  [ "$rc" -ge 2 ] && exit "$rc"     # usage / lock error — stop
  echo "run-fuzzer: chunk exited $rc — restarting (state resumes)" >&2
  sleep 5
done

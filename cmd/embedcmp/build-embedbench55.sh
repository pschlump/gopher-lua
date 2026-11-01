#!/bin/sh
# build-embedbench55.sh — build the NATIVE C embedding-API harness for
# cmd/embedcmp (docs/Embedded-EVAL-Perf.md): cmd/embedcmp/c/embedbench55.c,
# a custom main that drives stock C Lua 5.5.1 through its C API the way
# an application embeds it (one state, chunk loaded once, staged pcalls).
#
# Ported from glasm's runtime/build-embedbench55.sh at the N3b carve
# (flags identical so this repo's numbers stay comparable with glasm's
# recorded runs). The vendored tree is cmd/embedcmp/c/lua55 (glasm's
# runtime/lua55, unmodified — see its VENDORED.md patch list).
#
# The C sources live under cmd/embedcmp/c/ (no .go files) because a Go
# package dir may not hold .c sources without cgo — `go build ./...`
# ignores the subdirectory entirely.
#
# Output: note/native-lua55/embedbench   (note/ is gitignored)
set -e
cd "$(dirname "$0")/../.."

SRC=cmd/embedcmp/c/lua55/src
OUT=note/native-lua55

mkdir -p "$OUT"
clang -O2 -Wall -DLUA_USE_MACOSX -I"$SRC" \
  -o "$OUT/embedbench" cmd/embedcmp/c/embedbench55.c "$SRC"/*.c \
  -lreadline -lm

# smoke: one reuse + one fresh run over a trivial staged script
T=$(mktemp /tmp/embedbench-smoke.XXXXXX)
printf 'return #KEYS + #ARGV\n' >"$T"
"$OUT/embedbench" reuse "$T" 3 >/dev/null
"$OUT/embedbench" fresh "$T" 3 >/dev/null
rm -f "$T"

echo "built $OUT/embedbench"

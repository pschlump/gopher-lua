#!/bin/sh
# runtime/tests/run.sh — native rt_* ABI unit tests (L1/L2 of the plan).
# Builds the runtime + ABI for the host and runs the test matrix:
#   plain, ASan, UBSan. The wasm-side gates (layout + seam) live in
#   wasm/sharedmem_test.go and testdiff/rtseam_test.go.
set -e
cd "$(dirname "$0")/.."

CC=${CC:-clang}
SRC="tests/rt_native_test.c lua51/src/*.c"
CFLAGS="-DLUA_CORE -DRT_ABI_NATIVE64 -DNDEBUG -I."

run_build() {
  tag=$1; shift
  $CC $CFLAGS "$@" -o tests/rt_native_$tag $SRC -lm
}

echo "== plain"
run_build plain && ./tests/rt_native_plain

echo "== ASan"
run_build asan -fsanitize=address -fno-omit-frame-pointer && ./tests/rt_native_asan

echo "== UBSan"
run_build ubsan -fsanitize=undefined && UBSAN_OPTIONS=halt_on_error=1 ./tests/rt_native_ubsan

rm -f tests/rt_native_plain tests/rt_native_asan tests/rt_native_ubsan

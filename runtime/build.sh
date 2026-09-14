#!/bin/sh
# runtime/build.sh — builds the C-Lua oracle module.
#
# Default: lua51_sjlj.wasm — stock Lua 5.1.5 + luawasm.c glue with native
# EH-based setjmp/longjmp (-mllvm -wasm-enable-sjlj). Runs on engines that
# implement the wasm EH proposal (wasmtime, wasmer, node); NOT on wazero.
# This is the module the testdiff oracle uses (option (b), plan doc §M2).
#
# --with-asyncify: also builds selftest.wasm and lua51.wasm — the
# Asyncify-based setjmp experiments (runtime/setjmp_asyncify.c) that run
# on core-wasm engines like wazero. Kept as documentation of that path's
# constraints; the setjmp gate in testdiff skips when selftest.wasm is
# absent.
#
# Requires wasi-sdk (WASI_SDK=...) and, for --with-asyncify, binaryen.
set -e
cd "$(dirname "$0")"

WASI_SDK=${WASI_SDK:-$HOME/wasi-sdk-dl/wasi-sdk-34.0-arm64-macos}
WASM_OPT=${WASM_OPT:-wasm-opt}
CC="$WASI_SDK/bin/clang"
SYSROOT="$WASI_SDK/share/wasi-sysroot"

SRC=$(ls lua51/src/*.c)

EXPORTS="-Wl,--export=lnewstate -Wl,--export=lclose -Wl,--export=ldostring \
  -Wl,--export=lerrlen -Wl,--export=lerrcopy -Wl,--export=linbuf \
  -Wl,--export=lnamebuf -Wl,--export=lmalloctest -Wl,--export=ldiag_newstate \
  -Wl,--export=ldiag_openlibs -Wl,--export=ldiag_shims -Wl,--export=ldiag_stage \
  -Wl,--export=ldiag_lstate -Wl,--export=ldiag_sj \
  -Wl,--export=rt_abi_version -Wl,--export=rt_set_state -Wl,--export=rt_mknumber \
  -Wl,--export=rt_mkbool -Wl,--export=rt_mknil -Wl,--export=rt_intern \
  -Wl,--export=rt_newtable -Wl,--export=rt_gettable -Wl,--export=rt_settable \
  -Wl,--export=rt_err_pending -Wl,--export=rt_err_clear -Wl,--export=rt_err_stage_copy \
  -Wl,--export=rt_arith -Wl,--export=rt_len -Wl,--export=rt_eq -Wl,--export=rt_lt \
  -Wl,--export=rt_le -Wl,--export=rt_concat -Wl,--export=rt_call -Wl,--export=rt_call_count \
  -Wl,--export=rt_forprep -Wl,--export=rt_error -Wl,--export=rt_frame_alloc \
  -Wl,--export=rt_getglobal -Wl,--export=rt_setglobal -Wl,--export=rt_set_chunkname \
  -Wl,--export=lglobals"

# ---- lua51_sjlj.wasm: native EH setjmp, runs on wasmtime ----

"$CC" --sysroot="$SYSROOT" \
  -O2 -DNDEBUG -mexec-model=reactor \
  -DLUAWASM_SJLJ \
  -mllvm -wasm-enable-sjlj -mllvm -wasm-use-legacy-eh=false \
  -lsetjmp \
  $EXPORTS \
  -Wl,-z,stack-size=8388608 -Wl,--strip-all \
  -o lua51_sjlj.wasm luawasm.c rt_abi.c $SRC

# ---- asyncify variants (documentation path; wazero-compatible) ----

if [ "$1" = "--with-asyncify" ]; then
  ASYNCIFY_CFLAGS="-Ishim -O1 -DNDEBUG -mexec-model=reactor"
  ASYNCIFY="$WASM_OPT --asyncify -O1 --pass-arg=asyncify-ignore-imports"

  "$CC" --sysroot="$SYSROOT" $ASYNCIFY_CFLAGS \
    -Wl,--export=selftest -Wl,--strip-all \
    -o selftest_raw.wasm selftest.c setjmp_asyncify.c
  $ASYNCIFY selftest_raw.wasm -o selftest.wasm
  rm -f selftest_raw.wasm

  "$CC" --sysroot="$SYSROOT" $ASYNCIFY_CFLAGS \
    $EXPORTS -Wl,--strip-all \
    -o lua51_raw.wasm luawasm.c setjmp_asyncify.c $SRC
  $ASYNCIFY lua51_raw.wasm -o lua51.wasm
  rm -f lua51_raw.wasm

  cp selftest.wasm ../testdiff/selftest.wasm
  cp lua51.wasm ../testdiff/lua51.wasm
fi

cp lua51_sjlj.wasm ../testdiff/lua51_sjlj.wasm

ls -la lua51_sjlj.wasm
[ "$1" = "--with-asyncify" ] && ls -la selftest.wasm lua51.wasm
exit 0

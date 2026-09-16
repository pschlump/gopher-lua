/*
** runtime/tests/rt_concattri_test.c — row 27/31 triage: run the
** concat-loop shapes that corrupt wasm runs natively, under
** ASan/UBSan, to decide whether the corruption lives in the C ABI
** layer or in the wasm-side (emit / SJLJ / engine) layers.
**
** Shapes, in faithfulness order:
**   T1  concat of two constant cells into a frame dst, N times
**   T2  T1 + rt_settable of the result after each concat (t[i]="a".."b")
**   T3  T1 with a fresh interned operand each iteration (unique results)
**   T4  concat whose result feeds rt_call (print-shaped argument)
** Each shape runs N = 1..200 and reports the first N that fails, or
** all-pass. The wasm signature (16 dies, 15 and 100 pass) reproduces
** here only if the bug is native to the C layer.
**
** Build (mirrors run.sh, single TU with the Lua core):
**   clang -DLUA_CORE -DRT_ABI_NATIVE64 -DNDEBUG -I.. \
**     -fsanitize=address,undefined -fno-omit-frame-pointer \
**     -o /tmp/rt_concattri tests/rt_concattri_test.c lua51/src/*.c -lm
*/

#include <math.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "../rt_abi.c"

int32_t wasm_dispatch_host(int32_t idx, rt_addr frame, rt_addr cl,
                           int32_t nargs, int32_t want) {
  (void)idx; (void)frame; (void)cl; (void)nargs; (void)want;
  return RTW_REFUSED;
}

static lua_State *L;
static int verbose = 0;

#define CELL(f, k) ((rt_addr)(uintptr_t)((f) + (k)))

/* one shaped run; returns the iteration at which it first failed, or
   -1 when the whole run (and the post-run sanity check) verified clean */
static long run_shape(int shape, long n) {
  /* a frame like the engine's rt_frame_alloc region: cells + slack */
  TValue *frame = malloc(32 * sizeof(TValue));
  TValue *table_cell = malloc(sizeof(TValue));
  long i, fail_at = -1;
  int32_t st;
  char buf[64];

  /* ABI cells are not GC roots (the documented row-11 contract): any
     GC step — shape 4's rt_call, or even the luaC_checkGC inside
     lua_pushlstring itself — frees strings held only in cells (ASan-
     proven use-after-free in luaV_concat). Root them on the Lua stack
     BEFORE interning into cells — exactly what the backend owes every
     value whose cell is live. */
  lua_pushlstring(L, "a", 1);
  lua_pushlstring(L, "b", 1);
  rt_intern(CELL(frame, 0), (rt_addr)(uintptr_t)"a", 1);
  rt_intern(CELL(frame, 1), (rt_addr)(uintptr_t)"b", 1);
  rt_newtable((rt_addr)(uintptr_t)table_cell, 0, 0);

  for (i = 0; i < n; i++) {
    if (shape == 3) {
      /* fresh interned operand per iteration (unique strings) */
      int len = snprintf(buf, sizeof buf, "k%ld", i);
      rt_intern(CELL(frame, 1), (rt_addr)(uintptr_t)buf, len);
    }
    st = rt_concat(CELL(frame, 0), 2, CELL(frame, 2), 2);
    if (st != RT_OK) { if (verbose) printf("  shape%d n=%ld: iter %ld rt_concat=%d\n", shape, n, i, st); fail_at = i; break; }
    if (!ttisstring(&frame[2])) { if (verbose) printf("  shape%d n=%ld: iter %ld dst not string\n", shape, n, i); fail_at = i; break; }
    if (shape == 2) {
      /* t[i+1] = result, like the compiled SETTABLE after OP_CONCAT */
      rt_mknumber(CELL(frame, 3), (double)i + 1);
      st = rt_settable((rt_addr)(uintptr_t)table_cell, CELL(frame, 3), CELL(frame, 2), 3);
      if (st != RT_OK) { if (verbose) printf("  shape%d n=%ld: iter %ld rt_settable=%d\n", shape, n, i, st); fail_at = i; break; }
    }
    if (shape == 4) {
      /* concat result as a call argument (the print shape) — keep the
         callable stack-referenced across the call, like rt_native_test */
      TValue fcell;
      lua_getglobal(L, "type");
      fcell = *(L->top - 1);
      st = rt_call((rt_addr)(uintptr_t)&fcell, CELL(frame, 2), 1, 1, 4);
      L->top--; /* release the kept reference */
      if (st != RT_OK) {
        char eb[256];
        rt_err_stage_copy((rt_addr)(uintptr_t)eb, sizeof eb);
        if (verbose) printf("  shape%d n=%ld: iter %ld rt_call=%d err=%s\n", shape, n, i, st, eb);
        rt_err_clear();
        fail_at = i; break;
      }
      if (!ttisstring(&frame[2]) || strcmp(svalue(&frame[2]), "string") != 0) {
        if (verbose) printf("  shape%d n=%ld: iter %ld call result wrong\n", shape, n, i);
        fail_at = i; break;
      }
    }
  }

  /* post-run sanity: the state must still answer ordinary requests,
     including one more concat of the current operands */
  if (fail_at < 0) {
    st = rt_concat(CELL(frame, 0), 2, CELL(frame, 2), 2);
    int ok = st == RT_OK && ttisstring(&frame[2]);
    if (ok) {
      /* shape 3 re-interned operand 1: last value is k<n-1> */
      char want[64];
      if (shape == 3) snprintf(want, sizeof want, "ak%ld", n - 1);
      else snprintf(want, sizeof want, "ab");
      ok = strcmp(svalue(&frame[2]), want) == 0;
    }
    if (!ok) {
      if (verbose) printf("  shape%d n=%ld: post-run sanity failed (st=%d)\n", shape, n, st);
      fail_at = n;
    }
  }

  free(frame);
  free(table_cell);
  L->top -= 2; /* release the rooted operand references */
  return fail_at;
}

int main(int argc, char **argv) {
  int shape;
  verbose = argc > 1;

  L = luaL_newstate();
  if (L == NULL) { printf("TRI: no state\n"); return 2; }
  { /* shape 4 calls a library function (linit.c provides the symbol;
       the declaration lives outside lauxlib.h in 5.1) */
    extern LUALIB_API void luaL_openlibs(lua_State *L);
    luaL_openlibs(L);
  }
  rt_set_state((rt_addr)(uintptr_t)L);
  if (rt_abi_version() != LUA_RT_ABI) { printf("TRI: abi\n"); return 2; }

  for (shape = 1; shape <= 4; shape++) {
    long first_fail_iter = -1, first_fail_n = -1, n;
    for (n = 1; n <= 200; n++) {
      long at = run_shape(shape, n);
      if (at >= 0 && first_fail_iter < 0) { first_fail_iter = at; first_fail_n = n; }
    }
    if (first_fail_iter >= 0)
      printf("TRI shape %d: FIRST FAIL at n=%ld (iteration %ld)\n",
             shape, first_fail_n, first_fail_iter);
    else
      printf("TRI shape %d: all n=1..200 pass\n", shape);
  }

  printf("RT-CONCATTRI DONE\n");
  return 0;
}

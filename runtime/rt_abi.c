/*
** runtime/rt_abi.c — implementation of the frozen rt_* ABI (rt_abi.h).
** Thin, protected wrappers over stock Lua internals: every call either
** completes into caller cells or stages an error (rt_err_pending /
** rt_err_stage_copy / rt_err_clear); Lua's setjmp-based unwinding is
** contained here via luaD_rawrunprotected.
**
** M3 first batch: version, state, value construction, intern, newtable,
** gettable, settable, error staging. Remaining surface (arith, len, eq/
** lt/le, concat, closures/upvalues, call fallback) lands with the M3
** completion pass; nothing here may change signature or semantics without
** bumping LUA_RT_ABI.
**
** The instance is single-threaded: work arguments cross through static
** storage. All pointers are i32 addresses in the shared linear memory.
*/

#include <stdint.h>
#include <string.h>

#include "rt_abi.h"
#include "lua51/src/lauxlib.h"
#include "lua51/src/ldo.h"
#include "lua51/src/lobject.h"
#include "lua51/src/lstate.h"
#include "lua51/src/lstring.h"
#include "lua51/src/ltable.h"
#include "lua51/src/lvm.h"

/* layout contract of the whole ABI */
_Static_assert(sizeof(TValue) == 16, "TValue must be 16 bytes on wasm32");
_Static_assert(sizeof(lua_Number) == 8, "numbers are f64");

static lua_State *curL;

/* staged error: sticky until rt_err_clear; bytes for the frame chain */
static TValue err_value;
static char err_buf[1024];
static int err_buf_len, err_pending;

static void stage_error(void) {
  const TValue *ev = curL->top - 1;
  err_value = *ev;
  curL->top -= 1;
  const char *s = svalue(ev);
  size_t n = strlen(s);
  if (n > sizeof err_buf - 1) n = sizeof err_buf - 1;
  memcpy(err_buf, s, n);
  err_buf[n] = '\0';
  err_buf_len = (int)n;
  err_pending = 1;
}

/* ---- ABI surface ---- */

int32_t rt_abi_version(void) { return LUA_RT_ABI; }

void rt_set_state(int32_t p) {
  curL = (lua_State *)(size_t)p;
  err_pending = 0;
  err_buf_len = 0;
  setnilvalue(&err_value);
}

int32_t rt_err_pending(void) { return err_pending; }

void rt_err_clear(void) {
  err_pending = 0;
  err_buf_len = 0;
}

int32_t rt_err_stage_copy(int32_t dst, int32_t cap) {
  int32_t n = err_buf_len < cap ? err_buf_len : cap;
  if (n > 0) memcpy((void *)(size_t)dst, err_buf, (size_t)n);
  return n;
}

/* ---- value construction (backend inlines these; kept for tests) ---- */

void rt_mknumber(int32_t cell, double v) {
  setnvalue((TValue *)(size_t)cell, v);
}

void rt_mkbool(int32_t cell, int32_t b) {
  setbvalue((TValue *)(size_t)cell, b);
}

void rt_mknil(int32_t cell) { setnilvalue((TValue *)(size_t)cell); }

int32_t rt_intern(int32_t cell, int32_t ptr, int32_t len) {
  if (err_pending) return RT_ERR;
  TString *ts = luaS_newlstr(curL, (const char *)(size_t)ptr, (size_t)len);
  setsvalue(curL, (TValue *)(size_t)cell, ts);
  return RT_OK;
}

int32_t rt_newtable(int32_t cell, int32_t narr, int32_t nrec) {
  if (err_pending) return RT_ERR;
  Table *t = luaH_new(curL, (int)narr, (int)nrec);
  sethvalue(curL, (TValue *)(size_t)cell, t);
  return RT_OK;
}

/* ---- table access, protected: luaV_gettable/luaV_settable can raise
   and can call Lua (__index/__newindex functions) — the runtime's own
   lvm executes those, so no callback crosses the ABI ---- */

typedef void (*body_fn)(void);
static body_fn cur_body;

static void protect_trampoline(lua_State *L, void *ud) {
  (void)L; (void)ud;
  cur_body();
}

/* returns RT_OK, or RT_ERR with the error staged (message gets the
   script-position prefix, matching the oracle's error format) */
static int rt_run(body_fn fn, int32_t line) {
  if (err_pending) return RT_ERR;
  cur_body = fn;
  lua_lock(curL);
  int status = luaD_rawrunprotected(curL, protect_trampoline, NULL);
  lua_unlock(curL);
  if (status != 0) {
    if (line != 0) {
      /* prefix .. message (stack: msg) */
      luaO_pushfstring(curL, "%s:%d: ", "script", (int)line);
      luaV_concat(curL, 2, 0); /* stack: prefix..msg */
    }
    stage_error();
    return RT_ERR;
  }
  return RT_OK;
}

static TValue gt_t, gt_k, *gt_dst;

static void gettable_body(void) {
  luaD_checkstack(curL, 3);
  setobj2s(curL, curL->top, &gt_t); curL->top++;
  setobj2s(curL, curL->top, &gt_k); curL->top++;
  luaV_gettable(curL, curL->top - 2, curL->top - 1, curL->top - 2);
  *gt_dst = *(TValue *)(curL->top - 2);
  curL->top -= 2;
}

int32_t rt_gettable(int32_t tblcell, int32_t keycell, int32_t dstcell, int32_t line) {
  gt_t = *(TValue *)(size_t)tblcell;
  gt_k = *(TValue *)(size_t)keycell;
  gt_dst = (TValue *)(size_t)dstcell;
  return rt_run(gettable_body, line);
}

static TValue st_t, st_k, st_v;

static void settable_body(void) {
  luaD_checkstack(curL, 3);
  setobj2s(curL, curL->top, &st_t); curL->top++;
  setobj2s(curL, curL->top, &st_k); curL->top++;
  setobj2s(curL, curL->top, &st_v); curL->top++;
  luaV_settable(curL, curL->top - 3, curL->top - 2, curL->top - 1);
  curL->top -= 3;
}

int32_t rt_settable(int32_t tblcell, int32_t keycell, int32_t valcell, int32_t line) {
  st_t = *(TValue *)(size_t)tblcell;
  st_k = *(TValue *)(size_t)keycell;
  st_v = *(TValue *)(size_t)valcell;
  return rt_run(settable_body, line);
}

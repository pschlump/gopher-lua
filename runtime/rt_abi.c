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

#ifndef LUA_CORE
#define LUA_CORE /* we link core internals; ltm/lvm/luai_num* need it */
#endif
#include <stdint.h>
#include <stdlib.h>
#include <stdio.h>
#include <string.h>

#include "rt_abi.h"
#include "lua51/src/lauxlib.h"
#include "lua51/src/ldo.h"
#include "lua51/src/lfunc.h"
#include "lua51/src/lobject.h"
#include "lua51/src/lstate.h"
#include "lua51/src/ldebug.h"
#include "lua51/src/lstring.h"
#include "lua51/src/ltm.h"
#include "rt_wasm.h"

/* lvm.c's l_strcmp is static; same semantics (Lua 5.1.5) */
static int rt_strcmp(const TString *ls, const TString *rs) {
  const char *a = getstr(ls), *b = getstr(rs);
  size_t la = ls->tsv.len, lb = rs->tsv.len;
  size_t n = la < lb ? la : lb;
  int c = memcmp(a, b, n);
  if (c != 0) return c;
  return la < lb ? -1 : (la > lb ? 1 : 0);
}
#include "lua51/src/lua.h"
#include "lua51/src/ltable.h"
#include "lua51/src/lvm.h"

/* layout contract of the whole ABI */
_Static_assert(sizeof(TValue) == 16, "TValue must be 16 bytes on wasm32");
_Static_assert(sizeof(lua_Number) == 8, "numbers are f64");

static lua_State *curL;
static int rt_wasm_depth; /* adapter nesting (RTW_MAX_DEPTH guard, v3) */

/* ---- M5d: the gopher-lua message dialect ----
**
** The wasm engine enables it (rt_set_dialect(1)); the clua oracle keeps
** the stock C 5.1 texts (M2 gate untouched). Texts mirror the fork's
** RaiseError strings exactly (_vm.go/state.go/baselib.go):
**   arith:    "cannot perform <op> operation between <t1> and <t2>"
**   concat:   "cannot perform concat operation between <t1> and <t2>"
**   index:    "attempt to index a non-table object(<t>) with key '<k>'"
**   compare:  "attempt to compare <t1> with <t2>"
**   call:     "attempt to call a non-function object"
** Type names follow lValueNames (value.go). Position prefixes come from
** the rt line immediates: rt_run's prefix for core raises, and the
** activation line-stack (below) for error() levels / luaL_where. */
static int rt_dialect;

/* activation line-stack: one entry per LIVE rt_run — the current line of
   each nested wasm activation (the CallInfos carry none). luaL_where
   reads it top-down for error() levels. */
#define RT_LINE_MAX 256
static int rt_lines[RT_LINE_MAX];
static int rt_line_sp;

/* set when a position was already decided (error() built its prefix via
   the line-stack, or chose none at level 0) — rt_run then must not add
   its own prefix. */
static int rt_where_set;

void rt_set_dialect(int32_t d) { rt_dialect = (int)d; }
int rt_gopher_dialect(void) { return rt_dialect; }

void rt_where_mark(void) { rt_where_set = 1; }

int rt_line_depth(void) { return rt_line_sp; }
int rt_line_at(int32_t from_top) {
  int i = rt_line_sp - 1 - (int)from_top;
  if (i < 0 || i >= rt_line_sp) return 0;
  return rt_lines[i];
}

/* gopher type names (value.go lValueNames) */
const char *rt_gtypename_safe(const TValue *v) {
  static const char *names[] = {"nil",     "boolean", "?",       "number",
                                "string",  "table",   "function",
                                "userdata", "thread"};
  int t = ttype(v);
  if (t < 0 || t > 8) return "?";
  return names[t];
}

/* gopher key/value repr for index messages: key.String() — integers
   plain, others %.14g */
static void rt_grepr(char *buf, size_t cap, const TValue *v) {
  switch (ttype(v)) {
  case LUA_TNUMBER: {
    lua_Number n = nvalue(v);
    if (n == (lua_Number)(long long)n)
      snprintf(buf, cap, "%lld", (long long)n);
    else
      snprintf(buf, cap, "%.14g", (double)n);
    break;
  }
  case LUA_TSTRING:
    snprintf(buf, cap, "%s", svalue(v));
    break;
  case LUA_TBOOLEAN:
    snprintf(buf, cap, "%s", bvalue(v) ? "true" : "false");
    break;
  default:
    snprintf(buf, cap, "%s", rt_gtypename_safe(v));
  }
}

/* arith op names: gopher's event strings minus the underscores */
static const char *rt_gopname(int op) {
  switch (op) {
  case RT_OP_ADD: return "add";
  case RT_OP_SUB: return "sub";
  case RT_OP_MUL: return "mul";
  case RT_OP_DIV: return "div";
  case RT_OP_MOD: return "mod";
  case RT_OP_POW: return "pow";
  case RT_OP_UNM: return "unm";
  default: return "?";
  }
}

/* the gopher index message, from t and k (both in hand at the raise) */
void rt_gindex_error(lua_State *L, const TValue *t, const TValue *k) {
  char kb[128];
  rt_grepr(kb, sizeof kb, k);
  luaG_runerror(L, "attempt to index a non-table object(%s) with key '%s'",
                rt_gtypename_safe(t), kb);
}
int rt_wasm_ci(lua_State *L) {
  if (L->ci == NULL)
    return 0;
  return rt_wasm_ciframe(L->ci);
}

int rt_wasm_ciframe(CallInfo *ci) {
  Closure *cl;
  if (ci == NULL || !ttisfunction(ci->func))
    return 0;
  cl = clvalue(ci->func);
  return !cl->c.isC && cl->l.p->wasm_idx >= 0;
}

/* staged-error position prefix: the real chunk name (parity with the
   interpreter's "name:line:" format) */
static char chunk_name[256] = "script";
static int chunk_name_len = 6;

/* staged error: sticky until rt_err_clear. err_value carries the EXACT
   error TValue (the M5a fix — the adapter re-raises from it, so non-string
   objects survive); err_buf holds message bytes, filled only when the
   value is a string. err_prefixed: the raiser already decided the
   position (error() levels) or the value is non-string — no rt prefix. */
static TValue err_value;
static char err_buf[1024];
static int err_buf_len, err_pending, err_prefixed;

static void stage_error(void) {
  const TValue *ev = curL->top - 1;
  err_value = *ev;
  curL->top -= 1;
  err_buf_len = 0;
  err_buf[0] = '\0';
  /* sticky per in-flight error: the origin decided the position (or the
     value is non-string) — outer re-stagings (the adapter's re-raise
     caught by enclosing rt_runs) must not prefix again */
  err_prefixed = err_prefixed || rt_where_set;
  rt_where_set = 0;
  if (ttisstring(ev)) {
    const char *s = svalue(ev);
    size_t n = strlen(s);
    if (n > sizeof err_buf - 1) n = sizeof err_buf - 1;
    memcpy(err_buf, s, n);
    err_buf[n] = '\0';
    err_buf_len = (int)n;
  } else {
    err_prefixed = 1; /* non-string: the engine renders the value */
  }
  err_pending = 1;
}

/* ---- ABI surface ---- */

int32_t rt_abi_version(void) { return LUA_RT_ABI; }

void rt_set_state(rt_addr p) {
  curL = (lua_State *)(size_t)p;
  err_pending = 0;
  err_buf_len = 0;
  err_prefixed = 0;
  setnilvalue(&err_value);
  rt_wasm_depth = 0;
}

/* ---- M6c: sandbox globals lockdown (host flag, m6 plan §4.3) ----
**
** rt_sandbox(1) nils the untrusted-script surface out of _G: io, os,
** package, require, module, dofile, loadfile, load, loadstring, debug.
** Kept: string/table/math, pcall/xpcall/error/assert/select/unpack,
** collectgarbage, tostring/tonumber/type/rawget/rawset/rawequal/
** setmetatable/getmetatable/ipairs/pairs/next, print (host shim).
**
** Applies to the state set by rt_set_state (host calls lnewstate →
** rt_set_state → rt_sandbox). In the prod flavor (LUAWASM_PROD) the
** libs/entries compile out entirely and every name is already absent —
** rt_sandbox still runs (belt-and-suspenders) and returns 0. In the dev
** blob it removes the opened surface at runtime, closing ledger row 24
** by removal (no runtime compilation surface) wherever the host enables
** it. Returns the number of globals that were actually present (and are
** now nil) — dev opens 10, prod sees 0; tests pin both.
*/
static const char *const rt_sandbox_nil[] = {
    "io",    "os",     "package", "require", "module", "dofile",
    "loadfile", "load", "loadstring", "debug", NULL};

int32_t rt_sandbox(int32_t on) {
  int n = 0;
  if (!on || curL == NULL) return -1;
  for (const char *const *p = rt_sandbox_nil; *p != NULL; p++) {
    lua_getglobal(curL, *p);
    int was_there = !lua_isnil(curL, -1);
    lua_pop(curL, 1);
    if (was_there) {
      lua_pushnil(curL);
      lua_setglobal(curL, *p);
      n++;
    }
  }
  return n;
}

int32_t rt_err_pending(void) { return err_pending; }


void rt_err_clear(void) {
  err_pending = 0;
  err_buf_len = 0;
  err_prefixed = 0; /* the staging contract: sticky until CLEARED — a
                       cleared error must not suppress the prefix of the
                       next one (native ABI tests check staged bytes) */
}

/* Called from luaD_pcall's recovery (ldo.c): a Lua-level catch consumes
   the rt-staged error. Without this, the staging stays sticky after
   pcall catches and the next rt_* in the surviving wasm frame refuses
   instantly (found by M5c's error-through-tailcall-chain test: pcall
   caught, then the chunk's own print rt_call failed with the stale
   error). The A2 spike missed it — its driver chunk was C-interpreted,
   so nothing after the catch ever crossed the ABI. */
void rt_pcall_caught(void) {
  err_pending = 0;
  err_buf_len = 0;
  err_prefixed = 0;
  setnilvalue(&err_value);
}

int32_t rt_err_stage_copy(rt_addr dst, int32_t cap) {
  int32_t n = err_buf_len < cap ? err_buf_len : cap;
  if (n > 0) memcpy((void *)(size_t)dst, err_buf, (size_t)n);
  return n;
}

/* ---- value construction (backend inlines these; kept for tests) ---- */

void rt_mknumber(rt_addr cell, double v) {
  setnvalue((TValue *)(size_t)cell, v);
}

void rt_mkbool(rt_addr cell, int32_t b) {
  setbvalue((TValue *)(size_t)cell, b);
}

void rt_mknil(rt_addr cell) { setnilvalue((TValue *)(size_t)cell); }

int32_t rt_intern(rt_addr cell, rt_addr ptr, int32_t len) {
  if (err_pending) return RT_ERR;
  TString *ts = luaS_newlstr(curL, (const char *)(size_t)ptr, (size_t)len);
  setsvalue(curL, (TValue *)(size_t)cell, ts);
  return RT_OK;
}

int32_t rt_newtable(rt_addr cell, int32_t narr, int32_t nrec) {
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

static int rt_run_raw(body_fn fn, int32_t line);

static int rt_run(body_fn fn, int32_t line) { return rt_run_raw(fn, line); }

/* returns RT_OK, or RT_ERR with the error staged (message gets the
   script-position prefix, matching the oracle's error format) */
static int rt_run_raw(body_fn fn, int32_t line) {
  int prefixed;
  if (err_pending) return RT_ERR;
  cur_body = fn;
  if (rt_line_sp < RT_LINE_MAX) rt_lines[rt_line_sp] = (int)line;
  rt_line_sp++;
  lua_lock(curL);
  int status = luaD_rawrunprotected(curL, protect_trampoline, NULL);
  lua_unlock(curL);
  rt_line_sp--;
  if (status != 0) {
    stage_error(); /* value + message bytes; captures rt_where_set */
    prefixed = err_prefixed;
    if (line != 0 && !prefixed && err_buf_len > 0) {
      /* position prefix, like luaG_runerror's — done in the buffer, not
         on the Lua stack: post-error stack discipline is fragile.
         Skipped when the raiser already decided the position
         (error() levels via the line-stack) and for non-string values
         (the engine renders those from the value itself). */
      char tmp[sizeof err_buf];
      int w = snprintf(tmp, sizeof tmp, "%.*s:%d: %s", chunk_name_len,
                       chunk_name, (int)line, err_buf);
      if (w > 0) {
        size_t n = (size_t)w;
        if (n >= sizeof tmp) n = sizeof tmp - 1;
        memcpy(err_buf, tmp, n);
        err_buf[n] = '\0';
        err_buf_len = (int)n;
        err_prefixed = 1; /* sticky: outer re-stagings must not re-prefix */
        /* row 28: pcall must catch the PREFIXED string too — the staged
           value is what the adapter re-raises and luaD_pcall hands back
           to the script, so rebuild it from the prefixed bytes (GC is
           stopped per run; the fresh TString is interned and linked). */
        if (ttisstring(&err_value))
          setsvalue(curL, &err_value,
                    luaS_newlstr(curL, err_buf, (size_t)err_buf_len));
      }
    }
    return RT_ERR;
  }
  return RT_OK;
}

static TValue gt_t, gt_k, *gt_dst;

static void gettable_body(void) {
  TValue *dst = gt_dst; /* reentrancy: an __index metamethod on a wasm
                           closure re-enters the ABI and overwrites the
                           statics before we read them back */
  luaD_checkstack(curL, 3);
  setobj2s(curL, curL->top, &gt_t); curL->top++;
  setobj2s(curL, curL->top, &gt_k); curL->top++;
  luaV_gettable(curL, curL->top - 2, curL->top - 1, curL->top - 2);
  *dst = *(TValue *)(curL->top - 2);
  curL->top -= 2;
}

int32_t rt_gettable(rt_addr tblcell, rt_addr keycell, rt_addr dstcell, int32_t line) {
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

int32_t rt_settable(rt_addr tblcell, rt_addr keycell, rt_addr valcell, int32_t line) {
  st_t = *(TValue *)(size_t)tblcell;
  st_k = *(TValue *)(size_t)keycell;
  st_v = *(TValue *)(size_t)valcell;
  return rt_run(settable_body, line);
}


/* ---- arithmetic (OP_*-coded; number fast path, string coercion,
   metamethods through the runtime's own dispatch) ---- */

static TValue ar_l, ar_r, *ar_dst;
static int ar_op;

static void arith_body(void) {
  TValue ln, rn;
  TValue *dst = ar_dst; /* reentrancy: a metamethod may re-enter the ABI */
  int op = ar_op;
  /* luaV_tonumber returns the converted value (the original cell when
     already a number, the temp only for strings) or NULL */
  const TValue *x = luaV_tonumber(&ar_l, &ln);
  const TValue *y = luaV_tonumber(&ar_r, &rn);
  if (x != NULL && y != NULL) {
    lua_Number a = nvalue(x), b = nvalue(y), res;
    switch (op) {
    case RT_OP_ADD: res = a + b; break;
    case RT_OP_SUB: res = a - b; break;
    case RT_OP_MUL: res = a * b; break;
    case RT_OP_DIV: res = a / b; break;
    case RT_OP_MOD: res = luai_nummod(a, b); break;
    case RT_OP_POW: res = luai_numpow(a, b); break;
    case RT_OP_UNM: res = -a; break;
    default: luaG_runerror(curL, "rt_abi: bad arith op %d", op); return;
    }
    setnvalue(dst, res);
    return;
  }
  /* non-numbers: metamethod, else the arith error (raises) — the
     gopher dialect carries the op and both type names (_vm.go) */
  TMS tm = (TMS)(op - RT_OP_ADD + TM_ADD);
  const TValue *tmf = luaT_gettmbyobj(curL, &ar_l, tm);
  if (ttisnil(tmf)) tmf = luaT_gettmbyobj(curL, &ar_r, tm);
  if (ttisnil(tmf)) {
    if (rt_dialect) {
      if (op == RT_OP_UNM)  /* the fork's own text (_vm.go OP_UNM) */
        luaG_runerror(curL, "__unm undefined");
      else
        luaG_runerror(curL, "cannot perform %s operation between %s and %s",
                      rt_gopname(op), rt_gtypename_safe(&ar_l),
                      rt_gtypename_safe(&ar_r));
    }
    else
      luaG_aritherror(curL, &ar_l, &ar_r);
  } else {
    luaD_checkstack(curL, 4);
    setobj2s(curL, curL->top, tmf); curL->top++;
    setobj2s(curL, curL->top, &ar_l); curL->top++;
    setobj2s(curL, curL->top, &ar_r); curL->top++;
    luaD_call(curL, curL->top - 3, 1);
    *dst = *(TValue *)(curL->top - 1);
    curL->top -= 1;
  }
}

int32_t rt_arith(int32_t op, rt_addr lhscell, rt_addr rhscell, rt_addr dstcell, int32_t line) {
  ar_op = (int)op;
  ar_l = *(TValue *)(size_t)lhscell;
  ar_r = *(TValue *)(size_t)rhscell;
  ar_dst = (TValue *)(size_t)dstcell;
  return rt_run(arith_body, line);
}

/* ---- length (#) ---- */

static TValue len_v, *len_dst;

static void len_body(void) {
  TValue *dst = len_dst; /* reentrancy (metamethod) */
  const TValue *tm = luaT_gettmbyobj(curL, &len_v, TM_LEN);
  if (!ttisnil(tm)) {  /* __len metamethod wins for any type (OP_LEN) */
    luaD_checkstack(curL, 3);
    setobj2s(curL, curL->top, tm); curL->top++;
    setobj2s(curL, curL->top, &len_v); curL->top++;
    luaD_call(curL, curL->top - 2, 1);
    *dst = *(TValue *)(curL->top - 1);
    curL->top -= 1;
    return;
  }
  switch (ttype(&len_v)) {
  case LUA_TTABLE:
    setnvalue(dst, cast_num(luaH_getn(hvalue(&len_v))));
    return;
  case LUA_TSTRING:
    setnvalue(dst, cast_num(tsvalue(&len_v)->len));
    return;
  default:
    luaG_typeerror(curL, &len_v, "get length of");
  }
}

int32_t rt_len(rt_addr cell, rt_addr dstcell, int32_t line) {
  len_v = *(TValue *)(size_t)cell;
  len_dst = (TValue *)(size_t)dstcell;
  return rt_run(len_body, line);
}

/* ---- comparisons (dst gets a boolean TValue) ---- */

static TValue cmp_a, cmp_b, *cmp_dst;
static int cmp_op; /* 0 = ==, 1 = <, 2 = <= */

static int call_tm2(const TValue *tm, const TValue *a, const TValue *b) {
  /* call tm(a,b) and return its truthiness; stack restored */
  int res;
  luaD_checkstack(curL, 4);
  setobj2s(curL, curL->top, tm); curL->top++;
  setobj2s(curL, curL->top, a); curL->top++;
  setobj2s(curL, curL->top, b); curL->top++;
  luaD_call(curL, curL->top - 3, 1);
  res = !l_isfalse(curL->top - 1);
  curL->top -= 1;
  return res;
}

static void cmp_body(void) {
  int res;
  TValue *dst = cmp_dst; /* reentrancy (metamethod) */
  switch (cmp_op) {
  case 0:
    res = (ttype(&cmp_a) == ttype(&cmp_b)) ? luaV_equalval(curL, &cmp_a, &cmp_b) : 0;
    break;
  case 1:
    res = luaV_lessthan(curL, &cmp_a, &cmp_b);
    break;
  default: /* <= : 5.1 semantics — __le, else mirrored __lt, else error */
    if (ttype(&cmp_a) != ttype(&cmp_b)) {
      res = luaG_ordererror(curL, &cmp_a, &cmp_b);
    } else if (ttisnumber(&cmp_a)) {
      res = nvalue(&cmp_a) <= nvalue(&cmp_b);
    } else if (ttisstring(&cmp_a)) {
      res = rt_strcmp(rawtsvalue(&cmp_a), rawtsvalue(&cmp_b)) <= 0;
    } else {
      const TValue *tm = luaT_gettmbyobj(curL, &cmp_a, TM_LE);
      if (!ttisnil(tm)) res = call_tm2(tm, &cmp_a, &cmp_b);
      else {
        tm = luaT_gettmbyobj(curL, &cmp_b, TM_LT);
        if (ttisnil(tm)) res = luaG_ordererror(curL, &cmp_a, &cmp_b);
        else res = !call_tm2(tm, &cmp_b, &cmp_a);
      }
    }
  }
  setbvalue(dst, res);
}

static int cmp_entry(int op, rt_addr acell, rt_addr bcell, rt_addr dstcell, int32_t line) {
  cmp_a = *(TValue *)(size_t)acell;
  cmp_b = *(TValue *)(size_t)bcell;
  cmp_dst = (TValue *)(size_t)dstcell;
  cmp_op = op;
  return rt_run(cmp_body, line);
}

int32_t rt_eq(rt_addr a, rt_addr b, rt_addr dst, int32_t line) { return cmp_entry(0, a, b, dst, line); }
int32_t rt_lt(rt_addr a, rt_addr b, rt_addr dst, int32_t line) { return cmp_entry(1, a, b, dst, line); }
int32_t rt_le(rt_addr a, rt_addr b, rt_addr dst, int32_t line) { return cmp_entry(2, a, b, dst, line); }

/* ---- concat: cells contiguous, lowest first ---- */

static TValue *cc_cells;
static int cc_n;
static TValue *cc_dst;

static void concat_body(void) {
  int i;
  TValue *dst = cc_dst; /* reentrancy (__concat may re-enter the ABI) */
  int n = cc_n;         /* and overwrite the statics before we read them */
  luaD_checkstack(curL, n + 1);
  for (i = 0; i < n; i++) {
    setobj2s(curL, curL->top, &cc_cells[i]);
    curL->top++;
  }
  luaV_concat(curL, n, cast_int(curL->top - curL->base) - 1); /* top n -> one */
  /* the result occupies the FIRST slot of the window; luaV_concat does
     not adjust L->top (its callers in lvm do): n values -> 1 result.
     Unlike lvm, the ABI's result lives in an off-stack cell, so the
     window must be fully popped — leaving the residual result TValue
     leaked one Lua-stack slot per call (ledger row 27: top crept past
     the frame until the run corrupted). */
  *dst = *(TValue *)(curL->top - n);
  curL->top -= n;
}

int32_t rt_concat(rt_addr cells, int32_t count, rt_addr dstcell, int32_t line) {
  if (count < 2) {
    if (count == 1) *(TValue *)(size_t)dstcell = *(TValue *)(size_t)cells;
    return RT_OK;
  }
  cc_cells = (TValue *)(size_t)cells;
  cc_n = (int)count;
  cc_dst = (TValue *)(size_t)dstcell;
  return rt_run(concat_body, line);
}

/* ---- calls: the universal fallback for dynamic callees. The callee may
   be a Lua closure (proto in shared memory; the runtime's lvm interprets
   it) or a C function. Results overwrite the arg cells; want<0 means
   multret and the negative result count is returned ---- */

static TValue ca_f;
static TValue *ca_args;
static int ca_n, ca_w, ca_nres;

/* Reentrancy law (the M5a callback machinery): the callee may be a wasm
   closure whose code re-enters the ABI — including another rt_call —
   before this body resumes, so NO static may be read after luaD_call.
   Everything lives on the C stack; only the single-word output
   (ca_nres) is written back at the very end. */
static void call_body(void) {
  int i, n = ca_n, w = ca_w, nres;
  TValue *args = ca_args;
  StkId base;
  luaD_checkstack(curL, n + 1);
  setobj2s(curL, curL->top, &ca_f); curL->top++;
  for (i = 0; i < n; i++) {
      setobj2s(curL, curL->top, &args[i]);
    curL->top++;
  }
  base = curL->top - n - 1;
  /* Ledger row 31: the callee may nest deep enough to grow — and MOVE —
     the Lua stack (luaD_checkstack inside the nested precall). A raw
     StkId dangles across that; only the offset survives (the same
     savestack/restorestack discipline precall_wasm uses). Reading the
     results or resetting top through a stale base corrupted every
     subsequent stack write — layout-dependent on whether realloc
     happened to move the block. */
  {
    ptrdiff_t baser = savestack(curL, base);
    luaD_call(curL, base, w < 0 ? LUA_MULTRET : w);
    base = restorestack(curL, baser);
  }
  nres = cast_int(curL->top - base);
  if (w >= 0) nres = w;
  for (i = 0; i < nres; i++) args[i] = base[i];
  curL->top = base;
  ca_nres = nres;
}

int32_t rt_call(rt_addr funcell, rt_addr argcells, int32_t nargs, int32_t want, int32_t line) {
  int w = (int)want, nres;
  ca_f = *(TValue *)(size_t)funcell;
  ca_args = (TValue *)(size_t)argcells;
  ca_n = (int)nargs;
  ca_w = w;
  ca_nres = 0;
  int st = rt_run(call_body, line);
  if (st != RT_OK) return RT_ERR;
  nres = ca_nres; /* read before encoding — a nested call may follow */
  return w < 0 ? -(nres + 1) : RT_OK; /* encode count; -1 means zero */
}

/* the count encoded by a multret rt_call */
int32_t rt_call_count(int32_t encoded) { return -(encoded + 1); }

/* ---- numeric-for preparation: coerce 3 cells in place; messages match
   the interpreter's (_vm.go OP_FORPREP) ---- */

static TValue *fp_cells;

static void forprep_body(void) {
  static const char *names[3] = {"init", "limit", "step"};
  int i;
  for (i = 0; i < 3; i++) {
    TValue nv;
    /* luaV_tonumber returns the converted value as a POINTER (the cell
       itself when already a number; the temp only for strings) — the
       value must be read through it, never from the unfilled temp */
    const TValue *x = luaV_tonumber(&fp_cells[i], &nv);
    if (x == NULL)
      luaG_runerror(curL, "for statement %s must be a number", names[i]);
    setnvalue(&fp_cells[i], nvalue(x));
  }
}

int32_t rt_forprep(rt_addr cells, int32_t line) {
  fp_cells = (TValue *)(size_t)cells;
  return rt_run(forprep_body, line);
}

/* ---- scratch: frame/cell regions in the shared heap. v1: malloc-backed;
   the arena-reset lifecycle arrives with the host integration (M7) ---- */

int32_t rt_frame_alloc(int32_t bytes) {
  void *p = malloc((size_t)bytes);
  return (int32_t)(size_t)p;
}

/* ---- raising from emitted code ---- */

void rt_error(rt_addr msgptr, int32_t msglen, int32_t line) {
  int w = snprintf(err_buf, sizeof err_buf, "%.*s:%d: %.*s",
                   chunk_name_len, chunk_name, (int)line,
                   (int)msglen, (const char *)(size_t)msgptr);
  err_buf_len = w > 0 ? w : 0;
  err_pending = 1;
  setnilvalue(&err_value);
}

/* ---- globals (ABI v2) ---- */

static TValue gg_k, *gg_dst, sg_k, sg_v;

static void getglobal_body(void) {
  luaD_checkstack(curL, 2);
  const char *name = svalue(&gg_k);
  lua_getglobal(curL, name);
  *gg_dst = *(TValue *)(curL->top - 1);
  curL->top -= 1;
}

int32_t rt_getglobal(rt_addr dstcell, rt_addr keycell, int32_t line) {
  gg_k = *(TValue *)(size_t)keycell;
  gg_dst = (TValue *)(size_t)dstcell;
  return rt_run(getglobal_body, line);
}

static void setglobal_body(void) {
  luaD_checkstack(curL, 2);
  const char *name = svalue(&sg_k);
  setobj2s(curL, curL->top, &sg_v); curL->top++;
  lua_setglobal(curL, name); /* pops the value itself */
}

int32_t rt_setglobal(rt_addr keycell, rt_addr valcell, int32_t line) {
  sg_k = *(TValue *)(size_t)keycell;
  sg_v = *(TValue *)(size_t)valcell;
  return rt_run(setglobal_body, line);
}

void rt_set_chunkname(rt_addr ptr, int32_t len) {
  int32_t n = len;
  if (n > (int32_t)sizeof chunk_name - 1) n = (int32_t)sizeof chunk_name - 1;
  memcpy(chunk_name, (const void *)(size_t)ptr, (size_t)n);
  chunk_name[n] = '\0';
  chunk_name_len = (int)n;
}

const char *rt_chunkname_ptr(void) { return chunk_name; }
int32_t rt_chunkname_len(void) { return chunk_name_len; }

/* ---- ABI v3 (M5a): the wasm-proto registry, frame stack, closures ----
**
** Everything C-initiated (pcall, sort comparators, gsub replacements,
** __index on wasm closures) funnels through luaD_call → luaD_precall →
** precall_wasm (ldo.c), which pushes a frame here and dispatches through
** the host into the script module's lua_dispatch.
*/

struct rt_wasm_md {
  int numparams, isvararg, nupvalues, framecells;
  struct { int instack, idx; } *uv; /* capture descriptors (nupvalues) */
  Proto *proto;                     /* the registered C Proto (wasm_idx) */
};

static struct rt_wasm_md *rt_md;
static int rt_md_n, rt_md_cap;

int32_t rt_wasm_enter(int32_t idx) {
  (void)idx;
  if (rt_wasm_depth >= RTW_MAX_DEPTH) {
    /* the interpreter's message (state.go:1141); M5d pins wording and
       the depth divergence is ledgered (plan §7) */
    TString *ts = luaS_newlstr(curL, "stack overflow", 14);
    err_pending = 1;
    setsvalue(curL, &err_value, ts);
    err_buf_len = 14;
    memcpy(err_buf, "stack overflow", 15);
    return 1;
  }
  rt_wasm_depth++;
  return 0;
}

void rt_wasm_leave(void) {
  if (rt_wasm_depth > 0) rt_wasm_depth--;
}

/* frame stack: chunked bump region in shared memory; the M6 arena
   lifecycle replaces this. Chunks are malloc'd — never realloc'd — so
   live frames below the cursor can never move. */
struct rt_frchunk {
  struct rt_frchunk *prev;
  rt_addr base;
  uint32_t cap, used;
};
static struct rt_frchunk *rt_fr;

/* rt_push_frame_from: size, bump and arg-fill a frame from a TValue
   array (the adapter passes the L stack; the M5c tail restage passes
   the staging buffer). */
static rt_addr rt_push_frame_from(int mdidx, int nargs, const TValue *src) {
  struct rt_wasm_md *m;
  uint32_t need;
  rt_addr frame;
  int np, nv, i;

  m = &rt_md[mdidx];
  nv = (m->isvararg && nargs > m->numparams) ? nargs - m->numparams : 0;
  need = (uint32_t)sizeof(TValue) * (uint32_t)(m->framecells + 1 + nv);
  if (rt_fr == NULL || rt_fr->cap - rt_fr->used < need) {
    uint32_t cap = need > (1u << 20) ? need * 2 : (1u << 20);
    struct rt_frchunk *ch = (struct rt_frchunk *)malloc(sizeof *ch);
    void *mem = malloc(cap);
    if (ch == NULL || mem == NULL) {
      free(ch);
      free(mem);
      return 0;
    }
    ch->prev = rt_fr;
    ch->base = (rt_addr)(size_t)mem;
    ch->cap = cap;
    ch->used = 0;
    rt_fr = ch;
  }
  frame = rt_fr->base + rt_fr->used;
  rt_fr->used += need;

  /* copy args: params → frame+0.., extras → varargBase (above the
     nregs+4 register window; plan §3.2). Non-vararg protos drop extras,
     like stock luaD_precall. */
  np = nargs < m->numparams ? nargs : m->numparams;
  for (i = 0; i < np; i++)
    *(TValue *)(size_t)(frame + (rt_addr)sizeof(TValue) * i) = src[i];
  if (m->isvararg) {
    rt_addr vb = frame + (rt_addr)sizeof(TValue) * m->framecells;
    for (i = 0; i < nv; i++)
      *(TValue *)(size_t)(vb + (rt_addr)sizeof(TValue) * i) =
          src[m->numparams + i];
  }
  return frame;
}

rt_addr rt_wasm_push_frame(lua_State *L, StkId base, int mdidx, int nargs) {
  (void)L;
  return rt_push_frame_from(mdidx, nargs, base);
}

rt_addr rt_frame_cursor(void) {
  return rt_fr ? rt_fr->base + (rt_addr)rt_fr->used : 0;
}

void rt_frame_restore(rt_addr saved) {
  while (rt_fr != NULL && saved < rt_fr->base) {
    struct rt_frchunk *p = rt_fr->prev;
    free((void *)(size_t)rt_fr->base);
    free(rt_fr);
    rt_fr = p;
  }
  if (rt_fr != NULL)
    rt_fr->used = (uint32_t)(saved - rt_fr->base);
}

/* ---- M5c: tailcall staging ----
**
** A wasm-closure tailcall stages its call descriptor here and returns
** the -2 sentinel; lua_dispatch's restage loop turns it into a fresh
** dispatch at the SAME adapter level — O(1) wasm stack, O(1) frames
** (each restage reuses the memory the stager just restored). Anything
** that is not a registered wasm closure (C functions, __call'd objects)
** declines staging (-1) and the emitted code falls back to rt_call —
** precall's tryfuncTM resolves __call, and C tailcalls don't recurse
** (_vm.go:587-650 parity).
**
** Reentrancy: stage→restage is a closed cycle inside one lua_dispatch —
** the staged fields are consumed (copied into the new frame) before any
** nested dispatch can run, so overwrites are safe. */
static TValue *tail_args;
static int tail_nargs, tail_idx;
static rt_addr tail_cl;

int32_t rt_tail_stage(rt_addr funcell, rt_addr argcells, int32_t nargs,
                      rt_addr frame) {
  const TValue *f = (const TValue *)(size_t)funcell;
  Closure *cl;
  TValue *na;
  int i;
  if (err_pending) return -1;
  if (!ttisfunction(f) || clvalue(f)->c.isC) return -1;
  cl = clvalue(f);
  if (cl->l.p->wasm_idx < 0 || cl->l.p->wasm_idx >= rt_md_n) return -1;
  na = (TValue *)realloc(tail_args,
                         (size_t)(nargs > 0 ? nargs : 1) * sizeof(TValue));
  if (na == NULL) return -1;
  tail_args = na;
  for (i = 0; i < nargs; i++)
    tail_args[i] = *(TValue *)(size_t)(argcells + (rt_addr)sizeof(TValue) * i);
  tail_nargs = (int)nargs;
  tail_idx = cl->l.p->wasm_idx;
  tail_cl = (rt_addr)(size_t)cl;
  rt_frame_restore(frame); /* the tailcalling frame is dead */
  return tail_idx;
}

int32_t rt_tail_clidx(void) { return tail_idx; }
int32_t rt_tail_nargs(void) { return tail_nargs; }
rt_addr rt_tail_funcell(void) { return tail_cl; }

rt_addr rt_tail_restage(void) {
  return rt_push_frame_from(tail_idx, tail_nargs, tail_args);
}

/* rt_wasm_proto: protected the same way (luaF_newproto/luaS_newlstr
   allocate; OOM must not longjmp into wasm mid-init). */
static int32_t wp_idx, wp_numparams, wp_isvararg, wp_nupvalues, wp_framecells;
static Proto *wp_out;

static void wasm_proto_body(void) {
  Proto *p = luaF_newproto(curL); /* GC-linked */
  p->wasm_idx = (int)wp_idx;
  p->source = luaS_newlstr(curL, chunk_name, (size_t)chunk_name_len);
  p->numparams = (lu_byte)wp_numparams;
  p->is_vararg = (lu_byte)wp_isvararg;
  p->nups = (lu_byte)wp_nupvalues;
  /* maxstacksize is a lu_byte; framecells can exceed 255 (nregs+4). The
     ADAPTER sizes frames from the registry (full int); the Proto field
     is C-invariant bookkeeping only (checkstack/ci->top). */
  p->maxstacksize = (lu_byte)(wp_framecells > 255 ? 255 : wp_framecells);
  wp_out = p;
}

int32_t rt_wasm_proto(int32_t idx, int32_t numparams, int32_t isvararg,
                      int32_t nupvalues, int32_t framecells) {
  struct rt_wasm_md *m;
  if (err_pending) return RT_ERR;
  if (idx < 0) return RT_ERR;
  if (idx >= rt_md_cap) {
    int ncap = rt_md_cap ? rt_md_cap * 2 : 16;
    while (ncap <= idx) ncap *= 2;
    struct rt_wasm_md *nm =
        (struct rt_wasm_md *)realloc(rt_md, (size_t)ncap * sizeof *nm);
    if (nm == NULL) return RT_ERR;
    rt_md = nm;
    rt_md_cap = ncap;
  }
  wp_idx = idx;
  wp_numparams = numparams;
  wp_isvararg = isvararg;
  wp_nupvalues = nupvalues;
  wp_framecells = framecells;
  wp_out = NULL;
  {
    int st = rt_run(wasm_proto_body, 0);
    if (st != RT_OK) return st;
  }
  m = &rt_md[idx];
  m->numparams = numparams;
  m->isvararg = isvararg;
  m->nupvalues = nupvalues;
  m->framecells = framecells;
  m->uv = NULL;
  if (nupvalues > 0) {
    m->uv = (void *)calloc((size_t)nupvalues, sizeof *m->uv);
    if (m->uv == NULL) return RT_ERR;
  }
  m->proto = wp_out;
  if (idx >= rt_md_n) rt_md_n = idx + 1;
  return RT_OK;
}

void rt_wasm_upval(int32_t protoidx, int32_t uvidx, int32_t instack,
                   int32_t idx) {
  if (protoidx < 0 || protoidx >= rt_md_n) return;
  struct rt_wasm_md *m = &rt_md[protoidx];
  if (uvidx < 0 || uvidx >= m->nupvalues) return;
  m->uv[uvidx].instack = instack;
  m->uv[uvidx].idx = idx;
}

int32_t rt_wasm_count(void) { return rt_md_n; }

/* rt-owned open-upvalue registry (NOT L->openupval: ldo.c's correctstack
   re-bases every pointer in that list on stack growth, which would
   corrupt wasm frame addresses; luaF_close's numeric ordering would
   prematurely close outer wasm activations — plan §3.4). Address-ordered
   descending, like luaF_findupval's list discipline. UpVals themselves
   are luaF_newupval objects (GC-visible; registry-not-scanned is the
   ledgered v1 posture with GC stopped). */
struct rt_uvlink {
  struct rt_uvlink *next;
  rt_addr v;
  UpVal *uv;
};
static struct rt_uvlink *rt_openupval;

static UpVal *rt_findupval(rt_addr addr) {
  struct rt_uvlink **pp = &rt_openupval, *n;
  while (*pp != NULL && (*pp)->v >= addr) {
    if ((*pp)->v == addr) return (*pp)->uv;
    pp = &(*pp)->next;
  }
  n = (struct rt_uvlink *)malloc(sizeof *n);
  if (n == NULL) return NULL;
  n->uv = luaF_newupval(curL); /* closed-nil, GC-linked */
  n->uv->v = (StkId)(size_t)addr; /* open: points into the wasm frame */
  n->v = addr;
  n->next = *pp;
  *pp = n;
  return n->uv;
}

static void rt_closeuv(struct rt_uvlink *node) {
  setobj(curL, &node->uv->u.value, node->uv->v);
  node->uv->v = &node->uv->u.value; /* closed: the value lives here now */
  free(node);
}

void rt_close_upvals(rt_addr level) {
  while (rt_openupval != NULL && rt_openupval->v >= level) {
    struct rt_uvlink *n = rt_openupval->next;
    rt_closeuv(rt_openupval);
    rt_openupval = n;
  }
}

/* rt_newclosure: protected (allocation + UpVal creation can raise; no
   longjmp may cross into wasm). */
static rt_addr nc_dst, nc_parent, nc_frame;
static int32_t nc_protoidx;

static void newclosure_body(void) {
  struct rt_wasm_md *m = &rt_md[nc_protoidx];
  Closure *parent = nc_parent ? (Closure *)(size_t)nc_parent : NULL;
  Closure *cl;
  int i;
  cl = luaF_newLclosure(curL, m->nupvalues,
                        parent ? parent->l.env : hvalue(&curL->l_gt));
  /* luaF_newLclosure leaves l.p unset (stock callers set it — pushclosure
     in lvm.c); the adapter's precall hook reads cl->p->wasm_idx */
  cl->l.p = m->proto;
  for (i = 0; i < m->nupvalues; i++) {
    if (m->uv[i].instack) {
      UpVal *uv =
          rt_findupval(nc_frame + (rt_addr)sizeof(TValue) * m->uv[i].idx);
      if (uv == NULL) {
        luaG_runerror(curL, "not enough memory");
        return;
      }
      cl->l.upvals[i] = uv;
    } else if (parent != NULL) {
      cl->l.upvals[i] = parent->l.upvals[m->uv[i].idx];
    } else {
      luaG_runerror(curL,
                    "rt_newclosure: upvalue capture without a parent");
      return;
    }
  }
  setclvalue(curL, (TValue *)(size_t)nc_dst, cl);
}

int32_t rt_newclosure(rt_addr dstcell, int32_t protoidx, rt_addr parentcl,
                      rt_addr frameaddr, int32_t line) {
  if (err_pending || protoidx < 0 || protoidx >= rt_md_n) return RT_ERR;
  nc_dst = dstcell;
  nc_protoidx = protoidx;
  nc_parent = parentcl;
  nc_frame = frameaddr;
  return rt_run(newclosure_body, line);
}

void rt_getupval(rt_addr cl, int32_t idx, rt_addr cell) {
  UpVal *uv = ((Closure *)(size_t)cl)->l.upvals[idx];
  *(TValue *)(size_t)cell = *uv->v;
}

void rt_setupval(rt_addr cl, int32_t idx, rt_addr cell) {
  UpVal *uv = ((Closure *)(size_t)cl)->l.upvals[idx];
  *uv->v = *(TValue *)(size_t)cell; /* write-through: open → the frame cell */
}

/* the 5.0-compat `arg` table: {1..n, n=n} (state.go's initCallFrame) */
static TValue *ca_dst;
static rt_addr ca_vb;
static int ca_n;

static void compat_arg_body(void) {
  Table *t = luaH_new(curL, ca_n > 0 ? ca_n : 0, 1);
  int i;
  for (i = 0; i < ca_n; i++)
    setobj2t(curL, luaH_setnum(curL, t, i + 1),
             (TValue *)(size_t)(ca_vb + (rt_addr)sizeof(TValue) * i));
  {
    TValue v;
    setnvalue(&v, ca_n);
    setobj2t(curL, luaH_setstr(curL, t, luaS_newliteral(curL, "n")), &v);
  }
  sethvalue(curL, ca_dst, t);
}

int32_t rt_compat_arg(rt_addr dstcell, rt_addr varargbase, int32_t nvarargs) {
  if (err_pending) return RT_ERR;
  ca_dst = (TValue *)(size_t)dstcell;
  ca_vb = varargbase;
  ca_n = (int)nvarargs;
  return rt_run(compat_arg_body, 0);
}

int32_t rt_clidx(rt_addr funcell) {
  const TValue *f = (const TValue *)(size_t)funcell;
  if (ttisfunction(f) && !clvalue(f)->c.isC &&
      clvalue(f)->l.p->wasm_idx >= 0)
    return clvalue(f)->l.p->wasm_idx;
  return -1;
}

rt_addr rt_err_value_ptr(void) { return (rt_addr)(size_t)&err_value; }

int32_t rt_err_stage_value(rt_addr dst, int32_t cap) {
  if (cap < (int32_t)sizeof(TValue)) return 0;
  memcpy((void *)(size_t)dst, &err_value, sizeof err_value);
  return (int32_t)sizeof(TValue);
}

/* luaL_where hook (lauxlib.c): the level-th frame is a wasm closure →
   push "chunk:line: " from the activation line-stack and mark the
   position decided (rt_run must not re-prefix). Walking outward from
   L->ci, every wasm frame passed consumes one line-stack entry (its
   current rt_run's line); non-wasm frames just cost a level. Returns 0
   to let the stock path run. */
int rt_wasm_where(lua_State *L, int level) {
  CallInfo *ci = L->ci;
  int consumed = 0, idx;
  /* gopher's arithmetic (state.go where + skipg): the level-th frame,
     with C/G frames transparent — skip upward to the nearest wasm/Lua
     frame. Inner wasm frames each hold one line-stack entry (top-down);
     error('m',1) and ('m',2) both land on the direct caller because the
     raising C function itself occupies index 0. */
  for (idx = 0; ci >= L->base_ci; idx++, ci--) {
    Closure *cl = ttisfunction(ci->func) ? clvalue(ci->func) : NULL;
    int isw = cl != NULL && !cl->c.isC && cl->l.p->wasm_idx >= 0;
    if (isw) {
      if (idx >= level - 1) {
        int line = rt_line_at(consumed);
        if (line > 0) {
          lua_pushfstring(L, "%s:%d: ", chunk_name, line);
          rt_where_set = 1;
          return 1;
        }
        return 0;  /* no line info → stock */
      }
      consumed++;  /* inner wasm frame passed */
    }
    /* C frames: transparent — keep walking */
  }
  /* walked past the CI array: the main chunk runs without a CallInfo
     (the engine calls lua_main directly) but holds the bottom line-stack
     entry */
  if (consumed < rt_line_depth()) {
    int line = rt_line_at(consumed);
    if (line > 0) {
      lua_pushfstring(L, "%s:%d: ", chunk_name, line);
      rt_where_set = 1;
      return 1;
    }
  }
  return 0;
}

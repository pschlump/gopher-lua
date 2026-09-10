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
#include "lua51/src/lobject.h"
#include "lua51/src/lstate.h"
#include "lua51/src/ldebug.h"
#include "lua51/src/lstring.h"
#include "lua51/src/ltm.h"

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

/* staged-error position prefix: the real chunk name (parity with the
   interpreter's "name:line:" format) */
static char chunk_name[256] = "script";
static int chunk_name_len = 6;

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

static int rt_diag_lastfn = 0;
static int rt_diag_laststatus = -1;
static int rt_diag_errpending_entry = -1;
static int rt_diag_failfn = 0; /* fn id that FIRST returned RT_ERR */
int32_t rt_lastfn(void) { return rt_diag_lastfn; }
int32_t rt_failfn(void) { return rt_diag_failfn; }
int32_t rt_laststatus(void) { return rt_diag_laststatus; }

int32_t rt_abi_version(void) { return LUA_RT_ABI; }

void rt_set_state(rt_addr p) {
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

static int rt_run(body_fn fn, int32_t line) {
  int st = rt_run_raw(fn, line);
  rt_diag_laststatus = st;
  return st;
}

/* returns RT_OK, or RT_ERR with the error staged (message gets the
   script-position prefix, matching the oracle's error format) */
static int rt_run_raw(body_fn fn, int32_t line) {
  if (err_pending) return RT_ERR;
  cur_body = fn;
  lua_lock(curL);
  int status = luaD_rawrunprotected(curL, protect_trampoline, NULL);
  lua_unlock(curL);
  if (status != 0) {
    if (rt_diag_failfn == 0) rt_diag_failfn = rt_diag_lastfn;
    stage_error(); /* message bytes into err_buf */
    if (line != 0) {
      /* position prefix, like luaG_runerror's — done in the buffer, not
         on the Lua stack: post-error stack discipline is fragile */
      char tmp[sizeof err_buf];
      int w = snprintf(tmp, sizeof tmp, "%.*s:%d: %s", chunk_name_len,
                       chunk_name, (int)line, err_buf);
      if (w > 0) {
        size_t n = (size_t)w;
        if (n >= sizeof tmp) n = sizeof tmp - 1;
        memcpy(err_buf, tmp, n);
        err_buf[n] = '\0';
        err_buf_len = (int)n;
      }
    }
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

static int rt_diag_gtcalls = 0;
static int rt_diag_gt[4] = {-1, -1, -1, -1}; /* obj tag, key tag, dst tag, line */
int32_t rt_gtcalls(void) { return rt_diag_gtcalls; }
int32_t rt_gt(int32_t i) { return i >= 0 && i < 4 ? rt_diag_gt[i] : -1; }

int32_t rt_gettable(rt_addr tblcell, rt_addr keycell, rt_addr dstcell, int32_t line) {
  rt_diag_lastfn = 1;
  rt_diag_gtcalls++;
  rt_diag_gt[0] = (int)ttype((TValue *)(size_t)tblcell);
  rt_diag_gt[1] = (int)ttype((TValue *)(size_t)keycell);
  rt_diag_gt[2] = (int)ttype((TValue *)(size_t)dstcell);
  rt_diag_gt[3] = line;
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
  rt_diag_lastfn = 2;
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
  /* luaV_tonumber returns the converted value (the original cell when
     already a number, the temp only for strings) or NULL */
  const TValue *x = luaV_tonumber(&ar_l, &ln);
  const TValue *y = luaV_tonumber(&ar_r, &rn);
  if (x != NULL && y != NULL) {
    lua_Number a = nvalue(x), b = nvalue(y), res;
    switch (ar_op) {
    case RT_OP_ADD: res = a + b; break;
    case RT_OP_SUB: res = a - b; break;
    case RT_OP_MUL: res = a * b; break;
    case RT_OP_DIV: res = a / b; break;
    case RT_OP_MOD: res = luai_nummod(a, b); break;
    case RT_OP_POW: res = luai_numpow(a, b); break;
    case RT_OP_UNM: res = -a; break;
    default: luaG_runerror(curL, "rt_abi: bad arith op %d", ar_op); return;
    }
    setnvalue(ar_dst, res);
    return;
  }
  /* non-numbers: metamethod, else the standard arith error (raises) */
  TMS tm = (TMS)(ar_op - RT_OP_ADD + TM_ADD);
  const TValue *tmf = luaT_gettmbyobj(curL, &ar_l, tm);
  if (ttisnil(tmf)) tmf = luaT_gettmbyobj(curL, &ar_r, tm);
  if (ttisnil(tmf)) luaG_aritherror(curL, &ar_l, &ar_r);
  else {
    luaD_checkstack(curL, 4);
    setobj2s(curL, curL->top, tmf); curL->top++;
    setobj2s(curL, curL->top, &ar_l); curL->top++;
    setobj2s(curL, curL->top, &ar_r); curL->top++;
    luaD_call(curL, curL->top - 3, 1);
    *ar_dst = *(TValue *)(curL->top - 1);
    curL->top -= 1;
  }
}

int32_t rt_arith(int32_t op, rt_addr lhscell, rt_addr rhscell, rt_addr dstcell, int32_t line) {
  rt_diag_lastfn = 3;
  ar_op = (int)op;
  ar_l = *(TValue *)(size_t)lhscell;
  ar_r = *(TValue *)(size_t)rhscell;
  ar_dst = (TValue *)(size_t)dstcell;
  return rt_run(arith_body, line);
  ar_l = *(TValue *)(size_t)lhscell;
  ar_r = *(TValue *)(size_t)rhscell;
  ar_dst = (TValue *)(size_t)dstcell;
  return rt_run(arith_body, line);
}

/* ---- length (#) ---- */

static TValue len_v, *len_dst;

static void len_body(void) {
  switch (ttype(&len_v)) {
  case LUA_TTABLE:
    setnvalue(len_dst, cast_num(luaH_getn(hvalue(&len_v))));
    return;
  case LUA_TSTRING:
    setnvalue(len_dst, cast_num(tsvalue(&len_v)->len));
    return;
  default: {
    const TValue *tm = luaT_gettmbyobj(curL, &len_v, TM_LEN);
    if (ttisnil(tm)) luaG_typeerror(curL, &len_v, "get length of");
    luaD_checkstack(curL, 3);
    setobj2s(curL, curL->top, tm); curL->top++;
    setobj2s(curL, curL->top, &len_v); curL->top++;
    luaD_call(curL, curL->top - 2, 1);
    *len_dst = *(TValue *)(curL->top - 1);
    curL->top -= 1;
  }
  }
}

int32_t rt_len(rt_addr cell, rt_addr dstcell, int32_t line) {
  rt_diag_lastfn = 4;
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
  setbvalue(cmp_dst, res);
}

static int cmp_entry(int op, rt_addr acell, rt_addr bcell, rt_addr dstcell, int32_t line) {
  cmp_a = *(TValue *)(size_t)acell;
  cmp_b = *(TValue *)(size_t)bcell;
  cmp_dst = (TValue *)(size_t)dstcell;
  cmp_op = op;
  return rt_run(cmp_body, line);
}

int32_t rt_eq(rt_addr a, rt_addr b, rt_addr dst, int32_t line) {
  rt_diag_lastfn = 5; return cmp_entry(0, a, b, dst, line); }
int32_t rt_lt(rt_addr a, rt_addr b, rt_addr dst, int32_t line) { return cmp_entry(1, a, b, dst, line); }
int32_t rt_le(rt_addr a, rt_addr b, rt_addr dst, int32_t line) { return cmp_entry(2, a, b, dst, line); }

/* ---- concat: cells contiguous, lowest first ---- */

static TValue *cc_cells;
static int cc_n;
static TValue *cc_dst;

static void concat_body(void) {
  int i;
  luaD_checkstack(curL, cc_n + 1);
  for (i = 0; i < cc_n; i++) {
    setobj2s(curL, curL->top, &cc_cells[i]);
    curL->top++;
  }
  luaV_concat(curL, cc_n, cast_int(curL->top - curL->base) - 1); /* top n -> one */
  /* the result occupies the FIRST slot of the window; luaV_concat does
     not adjust L->top (its callers in lvm do): n values -> 1 result */
  *cc_dst = *(TValue *)(curL->top - cc_n);
  curL->top -= cc_n - 1;
}

int32_t rt_concat(rt_addr cells, int32_t count, rt_addr dstcell, int32_t line) {
  rt_diag_lastfn = 8;
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

static void call_body(void) {
  int i;
  StkId base;
  luaD_checkstack(curL, ca_n + 1);
  setobj2s(curL, curL->top, &ca_f); curL->top++;
  for (i = 0; i < ca_n; i++) {
      setobj2s(curL, curL->top, &ca_args[i]);
    curL->top++;
  }
  base = curL->top - ca_n - 1;
  luaD_call(curL, base, ca_w < 0 ? LUA_MULTRET : ca_w);
  ca_nres = cast_int(curL->top - base);
  if (ca_w >= 0) ca_nres = ca_w;
  for (i = 0; i < ca_nres; i++) ca_args[i] = base[i];
  curL->top = base;
}

static int rt_diag_ccalls = 0;
static int rt_diag_cargs[8] = {0};
int32_t rt_ccalls(void) { return rt_diag_ccalls; }
int32_t rt_carg(int32_t i) { return i >= 0 && i < 8 ? rt_diag_cargs[i] : -1; }

int32_t rt_call(rt_addr funcell, rt_addr argcells, int32_t nargs, int32_t want, int32_t line) {
  rt_diag_lastfn = 9;
  rt_diag_ccalls++;
  {
    int n = nargs < 4 ? nargs : 4;
    rt_diag_cargs[0] = (int)ttype((TValue *)(size_t)funcell);
    for (int i = 0; i < n; i++)
      rt_diag_cargs[i + 1] = (int)ttype(&((TValue *)(size_t)argcells)[i]);
    for (int i = n + 1; i < 8; i++) rt_diag_cargs[i] = -1;
  }
  ca_f = *(TValue *)(size_t)funcell;
  ca_args = (TValue *)(size_t)argcells;
  ca_n = (int)nargs;
  ca_w = (int)want;
  ca_nres = 0;
  int st = rt_run(call_body, line);
  if (st != RT_OK) return RT_ERR;
  return ca_w < 0 ? -(ca_nres + 1) : RT_OK; /* encode count; -1 means zero */
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

static int rt_diag_fpcalls = 0;
static double rt_diag_fpin[3] = {-999, -999, -999};
int32_t rt_fpcalls(void) { return rt_diag_fpcalls; }
double rt_fpin(int32_t i) { return i >= 0 && i < 3 ? rt_diag_fpin[i] : -999; }

int32_t rt_forprep(rt_addr cells, int32_t line) {
  rt_diag_lastfn = 10;
  rt_diag_fpcalls++;
  fp_cells = (TValue *)(size_t)cells;
  for (int i = 0; i < 3; i++) {
    const TValue *v = luaV_tonumber(&fp_cells[i], &(TValue){0});
    rt_diag_fpin[i] = v ? nvalue(v) : -777;
  }
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

static int rt_diag_ggcalls = 0;
static char rt_diag_ggname[64] = {0};
static int rt_diag_ggresult = -1; /* tag of the fetched value, -2 = error */
int32_t rt_ggcalls(void) { return rt_diag_ggcalls; }
const char *rt_ggname(void) { return rt_diag_ggname; }
int32_t rt_ggresult(void) { return rt_diag_ggresult; }

int32_t rt_getglobal(rt_addr dstcell, rt_addr keycell, int32_t line) {
  rt_diag_lastfn = 11;
  rt_diag_ggcalls++;
  {
    TValue *kv = (TValue *)(size_t)keycell;
    if (ttisstring(kv)) {
      size_t n = tsvalue(kv)->len;
      if (n > 63) n = 63;
      memcpy(rt_diag_ggname, svalue(kv), n);
      rt_diag_ggname[n] = 0;
    } else {
      strcpy(rt_diag_ggname, "<nonstring>");
    }
  }
  gg_k = *(TValue *)(size_t)keycell;
  gg_dst = (TValue *)(size_t)dstcell;
  int st = rt_run(getglobal_body, line);
  rt_diag_ggresult = st != 0 ? -2 : (int)ttype(gg_dst);
  return st;
}

static void setglobal_body(void) {
  luaD_checkstack(curL, 2);
  const char *name = svalue(&sg_k);
  setobj2s(curL, curL->top, &sg_v); curL->top++;
  lua_setglobal(curL, name); /* pops the value itself */
}

static int rt_diag_sgcalls = 0;
static int rt_diag_sgvaltag = -1;
static char rt_diag_sgname[64] = {0};
int32_t rt_sgvaltag(void) { return rt_diag_sgvaltag; }
int32_t rt_sgcalls(void) { return rt_diag_sgcalls; }
const char *rt_sgname(void) { return rt_diag_sgname; }

int32_t rt_setglobal(rt_addr keycell, rt_addr valcell, int32_t line) {
  rt_diag_lastfn = 12;
  rt_diag_sgcalls++;
  {
    TValue *kv = (TValue *)(size_t)keycell;
    if (ttisstring(kv)) {
      size_t n = tsvalue(kv)->len;
      if (n > 63) n = 63;
      memcpy(rt_diag_sgname, svalue(kv), n);
      rt_diag_sgname[n] = 0;
    } else {
      strcpy(rt_diag_sgname, "<nonstring>");
    }
  }
  rt_diag_sgvaltag = (int)ttype((TValue *)(size_t)valcell);
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

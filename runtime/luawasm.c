/*
** luawasm.c — driver glue for running stock Lua 5.1.5 inside wazero as the
** differential-testing oracle (docs/Lua-Wasm-Design-and-Test-Plan.md, M2).
**
** Division of labor: this file exposes raw primitives and a typed value
** encoding; all formatting lives in the Go host so both engines produce
** byte-identical event logs. Shims required by the harness contract
** (testdiff/interp.go installShim is the normative list):
**
**   print          -> typed buffer -> host.event (Go formats)
**   os.getenv      -> pure map {PATH:/bin:/usr/bin}, os.setenv writes it
**   os.execute     -> 1
**   os.time        -> 1234567890        os.clock -> 0
**   os.date        -> "2000-01-01 00:00:00"
**   os.tmpname     -> "testdiff.tmp"
**   math.random    -> host RNG (sequence shared with the interp engine)
**   math.randomseed-> host RNG seed
**
** Value protocol (tag-first, self-describing, little-endian):
**   tag byte: 0 nil, 1 false, 2 true, 3 number (f64), 4 string (u32 len,
**   bytes), 5 table (u32 nentries, then 2n values; PT_MORE sentinel if
**   truncated), 6 function, 7 userdata, 8 thread, 9 lightuserdata,
**   10 cycle, 11 depth-truncated table.
**
** Build: runtime/build.sh (wasi-sdk clang, reactor exec model).
*/

#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "lua51/src/lua.h"
#include "lua51/src/lauxlib.h"
#include "lua51/src/lualib.h"
#include "lua51/src/ldo.h"

#ifdef LUAWASM_SJLJ
/* Native build: wasi-sdk's EH-based setjmp (-mllvm -wasm-enable-sjlj);
   exports call Lua directly — no driver needed. Runs on engines that
   implement the wasm EH proposal (wasmtime), NOT on wazero. */
#define DRIVER_RUN(bodyfn, arg) bodyfn(arg)
#else
/* Asyncify build: see runtime/setjmp_asyncify.c. */
#include "shim/setjmp.h"
#include "setjmp_impl.h"
#define DRIVER_RUN(bodyfn, arg) ASYNC_DRIVER_RUN(bodyfn, arg)
#endif
#include "setjmp_impl.h"

/* ---- host imports ---- */

__attribute__((import_module("host"), import_name("event")))
void host_event(int32_t kind, int32_t ptr, int32_t len);

__attribute__((import_module("host"), import_name("random01")))
double host_random01(void);

__attribute__((import_module("host"), import_name("randomint")))
int32_t host_randomint(int32_t lo, int32_t hi);

__attribute__((import_module("host"), import_name("randomseed")))
void host_randomseed(int64_t seed);

/* ---- event kinds and value tags ---- */

enum { EVT_PRINT = 1, EVT_GLOBALS = 2 };

enum {
  PT_NIL = 0, PT_FALSE = 1, PT_TRUE = 2, PT_NUMBER = 3, PT_STRING = 4,
  PT_TABLE = 5, PT_FUNCTION = 6, PT_USERDATA = 7, PT_THREAD = 8,
  PT_LIGHTUSERDATA = 9, PT_CYCLE = 10, PT_DEEPTABLE = 11, PT_MORE = 12
};

#define EVBUF_CAP (256 * 1024)
#define ENC_MAX_DEPTH 8
#define ENC_MAX_ENTRIES 512

static char evbuf[EVBUF_CAP];

static int oob(int off, size_t n) { return off < 0 || off + n > EVBUF_CAP; }

static int enc_byte(int off, uint8_t b) {
  if (oob(off, 1)) return -1;
  evbuf[off] = (char)b;
  return off + 1;
}

static int enc_u32(int off, uint32_t v) {
  if (oob(off, 4)) return -1;
  memcpy(evbuf + off, &v, 4);
  return off + 4;
}

static int enc_f64(int off, double v) {
  if (oob(off, 8)) return -1;
  memcpy(evbuf + off, &v, 8);
  return off + 8;
}

static int enc_str(int off, const char *s, size_t len) {
  if (oob(off, 5 + len)) return -1;
  off = enc_u32(off, (uint32_t)len);
  memcpy(evbuf + off, s, len);
  return off + (int)len;
}

/* encode the value at stack index idx. tables expand when expand_tables,
   with cycle detection via the ancestor pointer stack. */
static int enc_value(lua_State *L, int idx, int off, int expand, int depth,
                     const void *ancestors[], int nanc) {
  int t = lua_type(L, idx);
  switch (t) {
  case LUA_TNIL: return enc_byte(off, PT_NIL);
  case LUA_TBOOLEAN: return enc_byte(off, lua_toboolean(L, idx) ? PT_TRUE : PT_FALSE);
  case LUA_TNUMBER: {
    off = enc_byte(off, PT_NUMBER);
    if (off < 0) return -1;
    return enc_f64(off, lua_tonumber(L, idx));
  }
  case LUA_TSTRING: {
    size_t len;
    const char *s = lua_tolstring(L, idx, &len);
    off = enc_byte(off, PT_STRING);
    if (off < 0) return -1;
    return enc_str(off, s, len);
  }
  case LUA_TTABLE: {
    if (!expand) return enc_byte(off, PT_TABLE);
    const void *p = lua_topointer(L, idx);
    for (int i = 0; i < nanc; i++)
      if (ancestors[i] == p) return enc_byte(off, PT_CYCLE);
    if (depth >= ENC_MAX_DEPTH) return enc_byte(off, PT_DEEPTABLE);
    if (nanc >= ENC_MAX_DEPTH) return enc_byte(off, PT_DEEPTABLE);
    /* first pass: count entries (lua_next normalizes the array part too) */
    int base = lua_gettop(L);
    int n = 0, truncated = 0;
    lua_pushvalue(L, idx); /* table copy for lua_next */
    lua_pushnil(L);
    while (lua_next(L, -2) != 0) {
      if (n < ENC_MAX_ENTRIES) n++; else truncated = 1;
      lua_pop(L, 1);
    }
    lua_settop(L, base);
    off = enc_byte(off, PT_TABLE);
    if (off < 0) return -1;
    off = enc_u32(off, (uint32_t)n);
    if (off < 0) return -1;
    if (truncated) {
      off = enc_byte(off, PT_MORE);
      if (off < 0) return -1;
    }
    ancestors[nanc] = p;
    base = lua_gettop(L);
    lua_pushvalue(L, idx);
    lua_pushnil(L);
    int written = 0;
    while (lua_next(L, -2) != 0 && written < n) {
      /* key at -2, value at -1 */
      off = enc_value(L, -2, off, 1, depth + 1, ancestors, nanc + 1);
      if (off < 0) { lua_settop(L, base); return -1; }
      off = enc_value(L, -1, off, 1, depth + 1, ancestors, nanc + 1);
      if (off < 0) { lua_settop(L, base); return -1; }
      lua_pop(L, 1);
      written++;
    }
    lua_settop(L, base);
    return off;
  }
  case LUA_TFUNCTION: return enc_byte(off, PT_FUNCTION);
  case LUA_TUSERDATA: return enc_byte(off, PT_USERDATA);
  case LUA_TTHREAD: return enc_byte(off, PT_THREAD);
  case LUA_TLIGHTUSERDATA: return enc_byte(off, PT_LIGHTUSERDATA);
  }
  return enc_byte(off, PT_NIL);
}

static int g_print(lua_State *L) {
  int n = lua_gettop(L);
  int off = enc_u32(0, (uint32_t)n); /* arg count header */
  if (off < 0) return 0;
  for (int i = 1; i <= n; i++) {
    off = enc_value(L, i, off, 0, 0, NULL, 0); /* print never expands tables */
    if (off < 0) return 0; /* oversized print: dropped, host sees header only */
  }
  host_event(EVT_PRINT, (int32_t)(size_t)evbuf, off);
  return 0;
}

static void emit_globals(lua_State *L) {
  const void *anc[ENC_MAX_DEPTH + 1];
  lua_getglobal(L, "_G");
  int off = enc_value(L, -1, 0, 1, 0, anc, 0);
  lua_pop(L, 1);
  if (off > 0) host_event(EVT_GLOBALS, (int32_t)(size_t)evbuf, off);
}

/* ---- deterministic environment map for os.getenv/os.setenv ---- */

typedef struct { char *k, *v; } envpair;
static envpair envmap[8];
static int envmap_n = 0;

static const char *env_lookup(const char *k) {
  for (int i = 0; i < envmap_n; i++)
    if (strcmp(envmap[i].k, k) == 0) return envmap[i].v;
  return NULL;
}

static void env_store(const char *k, const char *v) {
  for (int i = 0; i < envmap_n; i++) {
    if (strcmp(envmap[i].k, k) == 0) {
      free(envmap[i].v);
      envmap[i].v = strdup(v);
      return;
    }
  }
  if (envmap_n < 8) {
    envmap[envmap_n].k = strdup(k);
    envmap[envmap_n].v = strdup(v);
    envmap_n++;
  }
}

/* ---- math.random trampoline: sequence lives in the Go host ---- */

static int g_random(lua_State *L) {
  int n = lua_gettop(L);
  switch (n) {
  case 0:
    lua_pushnumber(L, host_random01());
    return 1;
  case 1: {
    int hi = luaL_checkint(L, 1);
    luaL_argcheck(L, hi >= 1, 1, "interval is empty");
    lua_pushnumber(L, host_randomint(1, hi));
    return 1;
  }
  default: {
    int lo = luaL_checkint(L, 1);
    int hi = luaL_checkint(L, 2);
    luaL_argcheck(L, lo <= hi, 2, "interval is empty");
    lua_pushnumber(L, host_randomint(lo, hi));
    return 1;
  }
  }
}

static int g_randomseed(lua_State *L) {
  host_randomseed((int64_t)luaL_checknumber(L, 1));
  return 0;
}

/* ---- constant os.* stubs ---- */

static void setfield(lua_State *L, const char *key, int value) {
  lua_pushstring(L, key);
  lua_pushnumber(L, value);
  lua_settable(L, -3);
}

static int getfield(lua_State *L, const char *key) {
  int res;
  lua_getfield(L, -1, key);
  res = (int)lua_tointeger(L, -1);
  lua_pop(L, 1);
  return res;
}

static int lua_getfield_isnum(lua_State *L, const char *key) {
  int isnum = 0;
  lua_getfield(L, -1, key);
  if (lua_isnumber(L, -1)) isnum = 1;
  lua_pop(L, 1);
  return isnum;
}

/* pinned instant: 2000-01-01 00:00:00 UTC = 946684800 */
#define PINNED_EPOCH 946684800.0

/* days from civil date (Howard Hinnant's algorithm) */
static long long days_from_civil(long long y, int m, int d) {
  y -= m <= 2;
  long long era = (y >= 0 ? y : y - 399) / 400;
  unsigned yoe = (unsigned)(y - era * 400);
  unsigned doy = (153u * (m + (m > 2 ? -3 : 9)) + 2) / 5 + d - 1;
  unsigned doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
  return era * 146097LL + (long long)doe - 719468LL;
}

static int g_gettime(lua_State *L) {
  if (lua_istable(L, 1)) {
    /* table form: real UTC calendar arithmetic (files.lua asserts time
       differences between table forms), plus normalization like real
       os.time (isdst back as a boolean) */
    lua_pushvalue(L, 1);
    int year = getfield(L, "year");
    int month = (int)getfield(L, "month");
    int day = (int)getfield(L, "day");
    int hour = lua_getfield_isnum(L, "hour") ? (int)getfield(L, "hour") : 12;
    int min = lua_getfield_isnum(L, "min") ? (int)getfield(L, "min") : 0;
    int sec = lua_getfield_isnum(L, "sec") ? (int)getfield(L, "sec") : 0;
    long long days = days_from_civil(year, month, day);
    long long t = days * 86400LL + hour * 3600LL + min * 60LL + sec;
    setfield(L, "hour", hour);
    setfield(L, "min", min);
    setfield(L, "sec", sec);
    lua_pushstring(L, "isdst");
    lua_pushboolean(L, 0);
    lua_settable(L, -3);
    lua_pop(L, 1);
    lua_pushnumber(L, (lua_Number)t);
    return 1;
  }
  /* no-arg: the pinned instant, consistent with os.date("*t") */
  lua_pushnumber(L, PINNED_EPOCH);
  return 1;
}



static int g_getclock(lua_State *L) {
  lua_pushnumber(L, 0);
  return 1;
}

static int g_getdate(lua_State *L) {
  const char *fmt = luaL_optstring(L, 1, "%c");
  if (fmt[0] == '!') fmt++; /* UTC == local for the pinned instant */
  if (strcmp(fmt, "*t") == 0) {
    /* table form for the pinned instant: 2000-01-01 00:00:00 UTC was a
       Saturday (Lua wday: 1=Sunday, %w: 0=Sunday) */
    lua_createtable(L, 0, 9);
    setfield(L, "year", 2000);
    setfield(L, "month", 1);
    setfield(L, "day", 1);
    setfield(L, "hour", 0);
    setfield(L, "min", 0);
    setfield(L, "sec", 0);
    setfield(L, "wday", 7);
    setfield(L, "yday", 1);
    lua_pushstring(L, "isdst");
    lua_pushboolean(L, 0);
    lua_settable(L, -3);
    return 1;
  }
  /* string form: a compact strftime over the pinned instant */
  char out[256];
  size_t o = 0;
  for (const char *p = fmt; *p != '\0' && o < sizeof out - 16; p++) {
    if (*p != '%' || p[1] == '\0') { out[o++] = *p; continue; }
    p++;
    switch (*p) {
    case 'Y': o += (size_t)snprintf(out + o, 8, "%d", 2000); break;
    case 'y': o += (size_t)snprintf(out + o, 8, "%02d", 0); break;
    case 'm': o += (size_t)snprintf(out + o, 8, "%02d", 1); break;
    case 'd': o += (size_t)snprintf(out + o, 8, "%02d", 1); break;
    case 'H': o += (size_t)snprintf(out + o, 8, "%02d", 0); break;
    case 'M': o += (size_t)snprintf(out + o, 8, "%02d", 0); break;
    case 'S': o += (size_t)snprintf(out + o, 8, "%02d", 0); break;
    case 'j': o += (size_t)snprintf(out + o, 8, "%03d", 1); break;
    case 'w': o += (size_t)snprintf(out + o, 8, "%d", 6); break;
    case 'W': case 'U': o += (size_t)snprintf(out + o, 8, "%02d", 0); break;
    case 'p': o += (size_t)snprintf(out + o, 8, "AM"); break;
    case 'A': o += (size_t)snprintf(out + o, 16, "Saturday"); break;
    case 'a': o += (size_t)snprintf(out + o, 8, "Sat"); break;
    case 'B': o += (size_t)snprintf(out + o, 16, "January"); break;
    case 'b': case 'h': o += (size_t)snprintf(out + o, 8, "Jan"); break;
    case 'c': o += (size_t)snprintf(out + o, 32, "Sat Jan  1 00:00:00 2000"); break;
    case 'x': o += (size_t)snprintf(out + o, 16, "01/01/00"); break;
    case 'X': o += (size_t)snprintf(out + o, 16, "00:00:00"); break;
    case '%': out[o++] = '%'; break;
    default: out[o++] = '%'; out[o++] = *p; break;
    }
  }
  out[o] = '\0';
  lua_pushstring(L, out);
  return 1;
}

static int tmpname_counter = 0;
static int g_tmpname(lua_State *L) {
  /* unique per call (files.lua renames tmpname() A to tmpname() B — a
     constant name would make that a rename onto itself); deterministic
     per module instance */
  char buf[64];
  snprintf(buf, sizeof buf, "testdiff.tmp.%d", ++tmpname_counter);
  lua_pushstring(L, buf);
  return 1;
}

static int g_getenv(lua_State *L) {
  const char *v = env_lookup(luaL_checkstring(L, 1));
  if (v == NULL) lua_pushnil(L);
  else lua_pushstring(L, v);
  return 1;
}

static int g_setenv(lua_State *L) {
  const char *k = luaL_checkstring(L, 1);
  const char *v = luaL_optstring(L, 2, NULL);
  if (v != NULL) env_store(k, v);
  lua_pushboolean(L, 1);
  return 1;
}

static int g_execute(lua_State *L) {
  lua_pushnumber(L, 1);
  return 1;
}

static int g_setlocale(lua_State *L) {
  /* WASI has no locales; wasi-libc's setlocale stub "succeeds" for any
     request, flipping the suite's locale-conditional tests. Behave like
     a C-only system: "C"/nil succeed, everything else fails. */
  const char *req = luaL_optstring(L, 1, "C");
  if (strcmp(req, "C") == 0 || strcmp(req, "c") == 0) {
    lua_pushliteral(L, "C");
  } else {
    lua_pushnil(L);
  }
  return 1;
}

/* WASI gaps: tmpfile/system are not provided. system returns -1 (the
   harness shims os.execute; corpus never calls it). tmpfile is backed by
   a unique deterministic name in the preopen FS (math.lua's io.tmpfile
   test needs a working file); the engine removes testdiff.tmp* after the
   run. */
static int tmpfile_counter = 0;
FILE *tmpfile(void) {
  char name[64];
  snprintf(name, sizeof name, "testdiff.tmp.file%d", ++tmpfile_counter);
  return fopen(name, "w+");
}
int system(const char *cmd) { (void)cmd; return -1; }

/* ---- host I/O staging buffers ---- */

static char inbuf[1 << 20];
static char namebuf[512];

int32_t linbuf(void) { return (int32_t)(size_t)inbuf; }

int32_t lmalloctest(int32_t n) { return (int32_t)(size_t)malloc((size_t)n); }
int32_t lnamebuf(void) { return (int32_t)(size_t)namebuf; }

/* ---- exported driver API ---- */

static void install_shims(lua_State *L) {
  env_store("PATH", "/bin:/usr/bin");

  lua_pushcfunction(L, g_print);
  lua_setglobal(L, "print");

  lua_getglobal(L, "math");
  lua_pushcfunction(L, g_random);
  lua_setfield(L, -2, "random");
  lua_pushcfunction(L, g_randomseed);
  lua_setfield(L, -2, "randomseed");
  lua_pop(L, 1);

  lua_getglobal(L, "os");
  lua_pushcfunction(L, g_gettime);  lua_setfield(L, -2, "time");
  lua_pushcfunction(L, g_getclock); lua_setfield(L, -2, "clock");
  lua_pushcfunction(L, g_getdate);  lua_setfield(L, -2, "date");
  lua_pushcfunction(L, g_tmpname);  lua_setfield(L, -2, "tmpname");
  lua_pushcfunction(L, g_getenv);   lua_setfield(L, -2, "getenv");
  lua_pushcfunction(L, g_setenv);   lua_setfield(L, -2, "setenv");
  lua_pushcfunction(L, g_execute);  lua_setfield(L, -2, "execute");
  lua_pushcfunction(L, g_setlocale); lua_setfield(L, -2, "setlocale");
  lua_pop(L, 1);
}

int32_t lnewstate(void) {
  lua_State *L = luaL_newstate();
  if (L == NULL) return 0;
  luaL_openlibs(L);
  install_shims(L);
  host_randomseed(42); /* harness contract: every run starts at seed 42 */
  return (int32_t)(size_t)L;
}

/* diagnostics: bisect lnewstate (must also run inside the driver —
   lua_newstate itself performs a protected call) */
int g_diag_stage;
int g_lstate_diag;

#ifndef LUAWASM_SJLJ
/* probe 1: setjmp called directly from an export-driven body (asyncify
   builds only) */
static luawasm_jmp_buf dsj_buf;
static void dsj_body(void *ud) {
  int *r = ud;
  g_diag_stage = 30;
  *r = luawasm_setjmp(dsj_buf);
  g_diag_stage = 31;
}
int32_t ldiag_dsj(void) {
  int r = -1;
  DRIVER_RUN(dsj_body, &r);
  return r;
}

static lua_State *diagL;

/* probe 2: Lua's real luaD_rawrunprotected from an export-driven body */
static int rrp_dummy_called = 0;
static void rrp_f(lua_State *L, void *ud) {
  (void)L; (void)ud;
  rrp_dummy_called++;
  g_diag_stage = 41;
}
static void rrp_body(void *ud) {
  int *r = ud;
  g_diag_stage = 40;
  *r = luaD_rawrunprotected(diagL, rrp_f, NULL);
  g_diag_stage = 42;
}
int32_t ldiag_rrp(int32_t p) {
  int r = -1;
  diagL = (lua_State *)(size_t)p;
  DRIVER_RUN(rrp_body, &r);
  return r;
}
#endif /* !LUAWASM_SJLJ */
int32_t ldiag_lstate(void) { return g_lstate_diag; }
static void diag_newstate_body(void *ud) {
  lua_State **out = ud;
  g_diag_stage = 1;
  *out = luaL_newstate();
  g_diag_stage = (*out != NULL) ? 3 : 2;
}
int32_t ldiag_stage(void) { return g_diag_stage; }
#ifndef LUAWASM_SJLJ
int32_t ldiag_sj(int32_t which) {
  extern int sj_diag_setjmp_calls, sj_diag_unwinds, sj_diag_rewinds;
  switch (which) {
  case 0: return sj_diag_setjmp_calls;
  case 1: return sj_diag_unwinds;
  default: return sj_diag_rewinds;
  }
}
#else
int32_t ldiag_sj(int32_t which) { (void)which; return 0; }
#endif
int32_t ldiag_newstate(void) {
  lua_State *L = NULL;
  DRIVER_RUN(diag_newstate_body, &L);
  return (int32_t)(size_t)L;
}
static lua_State *diag_L;
int32_t ldiag_openlibs(int32_t p) {
  diag_L = (lua_State *)(size_t)p;
  luaL_openlibs(diag_L);
  return diag_L != NULL ? 1 : 0;
}
int32_t ldiag_shims(int32_t p) {
  install_shims((lua_State *)(size_t)p);
  return 1;
}

void lclose(int32_t p) {
  lua_State *L = (lua_State *)(size_t)p;
  if (L != NULL) lua_close(L);
}

static char errdata[8192];
static int errlen = 0;

static void capture_error(lua_State *L) {
  errlen = 0;
  const char *m = lua_tostring(L, -1);
  if (m == NULL) m = "(non-string error object)";
  size_t n = strlen(m);
  if (n > sizeof errdata - 1) n = sizeof errdata - 1;
  memcpy(errdata, m, n);
  errlen = (int)n;
}

struct dostring_args {
  lua_State *L;
  const char *s;
  size_t len;
  const char *cn;
  int status;
};

static void dostring_body(void *ud) {
  struct dostring_args *a = ud;
  const char *s = a->s;
  size_t len = a->len;
  /* skip a leading '#' line, like the standalone interpreter's loader
     (both '#!/path/to/lua' and '# comment' forms) */
  if (len >= 1 && s[0] == '#') {
    while (len > 0 && s[0] != '\n') { s++; len--; }
    if (len > 0) { s++; len--; }
  }
  /* file-loader semantics: '@' + name (short_src and error positions then
     match "db.lua:28" instead of [string "db.lua"], like the standalone
     runner) */
  char cnbuf[600];
  const char *cn = a->cn;
  if (cn[0] != '@' && cn[0] != '=') {
    snprintf(cnbuf, sizeof cnbuf, "@%s", cn);
    cn = cnbuf;
  }
  a->status = luaL_loadbuffer(a->L, s, len, cn);
  if (a->status != 0) {
    capture_error(a->L);
    lua_pop(a->L, 1);
    return;
  }
  /* the standalone runner exposes the script name via arg; suite drivers
     read it */
  {
    lua_createtable(a->L, 0, 2);
    lua_pushstring(a->L, a->cn);
    lua_rawseti(a->L, -2, 0);
    lua_setglobal(a->L, "arg");
  }
  a->status = lua_pcall(a->L, 0, LUA_MULTRET, 0);
  if (a->status != 0) {
    capture_error(a->L);
    lua_pop(a->L, 1);
  }
}

/* ldostring loads+runs a chunk. Returns 0 on success, 1 on runtime error,
   2 on load/syntax error. On error the message is available via lerrlen/
   lerrcopy. */
int32_t ldostring(int32_t p, int32_t src, int32_t len, int32_t chunkname_ptr,
                  int32_t want_globals) {
  struct dostring_args a;
  a.L = (lua_State *)(size_t)p;
  a.s = (const char *)(size_t)src;
  a.len = (size_t)len;
  a.cn = (const char *)(size_t)chunkname_ptr;
  a.status = 0;
  errlen = 0;
  DRIVER_RUN(dostring_body, &a);
  if (a.status != 0) return a.status == LUA_ERRRUN ? 1 : 2;
  if (want_globals) emit_globals(a.L);
  return 0;
}

int32_t lerrlen(void) { return errlen; }

int32_t lerrcopy(int32_t dst, int32_t cap) {
  int32_t n = errlen < cap ? errlen : cap;
  if (n > 0) memcpy((void *)(size_t)dst, errdata, (size_t)n);
  return n;
}

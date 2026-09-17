/*
** runtime/tests/rt_native_test.c — native unit tests for the rt_* ABI
** (design doc §8.1 L1/L2): the same C sources compiled for the host,
** exercising the ABI surface directly. The wasm32-only concerns (TValue
** 16-byte layout, shared memory) are covered by the seam gates; these
** tests validate the semantics/protocol on a normal 64-bit host.
**
** Build & run: runtime/tests/run.sh (includes ASan+UBSan builds).
*/

#include <math.h>
#include <stdio.h>
#include <string.h>

/* pull the ABI in directly (single translation unit with Lua core) */
#include "../rt_abi.c"

/* wasm_dispatch_host is a host import on wasm; native tests have no
   script module, so stub it host-refused (nothing registers a wasm proto
   here, so precall_wasm never dispatches) */
int32_t wasm_dispatch_host(int32_t idx, rt_addr frame, rt_addr cl,
                           int32_t nargs, int32_t want) {
  (void)idx; (void)frame; (void)cl; (void)nargs; (void)want;
  return RTW_REFUSED;
}

static int failures = 0;
#define CHECK(cond) do { \
	if (!(cond)) { failures++; printf("FAIL %d: %s\n", __LINE__, #cond); } \
} while (0)

static rt_addr cell_of(TValue *v) { return (rt_addr)(uintptr_t)v; }

int main(void) {
  TValue c[16];
  rt_addr cc[16];
  int i;
  for (i = 0; i < 16; i++) cc[i] = cell_of(&c[i]);

  lua_State *L = luaL_newstate();
  CHECK(L != NULL);
  rt_set_state((rt_addr)(uintptr_t)L);
  CHECK(rt_abi_version() == LUA_RT_ABI);

  /* M6c sandbox: the dev build opens all libs, so rt_sandbox(1) must
     remove exactly the lockdown list (m6 plan §4.3) and keep the rest */
  {
    static const char *killed[] = {"io",     "os",     "package", "require",
                                   "module", "dofile", "loadfile", "load",
                                   "loadstring", "debug", NULL};
    int n, k;
    luaL_openlibs(L); /* dev-flavor surface for the sandbox to remove */
    n = rt_sandbox(1);
    CHECK(n == 10); /* every listed global was present and got nil'd */
    for (k = 0; killed[k]; k++) {
      lua_getglobal(L, killed[k]);
      CHECK(lua_isnil(L, -1));
      lua_pop(L, 1);
    }
    lua_getglobal(L, "pcall");
    CHECK(lua_isfunction(L, -1));
    lua_pop(L, 1);
    lua_getglobal(L, "string");
    CHECK(lua_istable(L, -1));
    lua_pop(L, 1);
    CHECK(rt_sandbox(0) == -1); /* off (and NULL-state) refused */
  }

  /* value construction */
  rt_mknumber(cc[0], 2.5);
  CHECK(ttisnumber(&c[0]) && nvalue(&c[0]) == 2.5);
  rt_mkbool(cc[1], 1);
  CHECK(ttisboolean(&c[1]) && bvalue(&c[1]) == 1);
  rt_mknil(cc[2]);
  CHECK(ttisnil(&c[2]));

  /* arith: all ops, string coercion, metamethod, error */
  rt_mknumber(cc[3], 7);
  CHECK(rt_arith(RT_OP_ADD, cc[0], cc[3], cc[4], 10) == RT_OK);
  CHECK(nvalue(&c[4]) == 9.5);
  CHECK(rt_arith(RT_OP_MUL, cc[0], cc[3], cc[4], 10) == RT_OK);
  CHECK(nvalue(&c[4]) == 17.5);
  CHECK(rt_arith(RT_OP_UNM, cc[0], cc[0], cc[4], 10) == RT_OK);
  CHECK(nvalue(&c[4]) == -2.5);
  CHECK(rt_arith(RT_OP_POW, cc[3], cc[0], cc[4], 10) == RT_OK);
  CHECK(nvalue(&c[4]) == pow(7, 2.5));
  rt_intern(cc[5], (rt_addr)(uintptr_t)"10", 2);
  CHECK(rt_arith(RT_OP_ADD, cc[5], cc[3], cc[6], 11) == RT_OK);
  CHECK(nvalue(&c[6]) == 17); /* "10" + 7 coerces */
  rt_mkbool(cc[7], 1);
  CHECK(rt_arith(RT_OP_ADD, cc[7], cc[7], cc[8], 12) == RT_ERR);
  CHECK(rt_err_pending() == 1);
  rt_err_clear();
  CHECK(rt_err_pending() == 0);

  /* tables: string keys, round trip, errors */
  CHECK(rt_newtable(cc[9], 0, 0) == RT_OK);
  rt_mknumber(cc[10], 42);
  CHECK(rt_settable(cc[9], cc[5], cc[10], 20) == RT_OK); /* t["10"]=42 */
  CHECK(rt_gettable(cc[9], cc[5], cc[11], 21) == RT_OK);
  CHECK(ttisnumber(&c[11]) && nvalue(&c[11]) == 42);
  rt_mknil(cc[12]);
  CHECK(rt_gettable(cc[12], cc[5], cc[11], 22) == RT_ERR); /* nil index */
  rt_err_clear();
  /* fractional keys are legal in the hash part (Lua 5.1 semantics) */
  CHECK(rt_settable(cc[9], cc[0], cc[10], 23) == RT_OK); /* t[2.5]=.. */

  /* length: string, table */
  CHECK(rt_len(cc[5], cc[13], 24) == RT_OK);
  CHECK(nvalue(&c[13]) == 2);
  CHECK(rt_settable(cc[9], cc[3], cc[10], 25) == RT_OK); /* t[7]=42 */
  /* not checking luaH_getn here: holes/resize semantics belong to the
     corpus; the ABI wrapper just forwards */

  /* comparisons */
  rt_mknumber(cc[14], 9.5);
  CHECK(rt_eq(cc[0], cc[0], cc[15], 30) == RT_OK);
  CHECK(ttisboolean(&c[15]) && bvalue(&c[15]) == 1);
  CHECK(rt_eq(cc[0], cc[14], cc[15], 30) == RT_OK);
  CHECK(ttisboolean(&c[15]) && bvalue(&c[15]) == 0);
  CHECK(rt_lt(cc[0], cc[14], cc[15], 31) == RT_OK);
  CHECK(bvalue(&c[15]) == 1); /* 2.5 < 9.5 */
  CHECK(rt_le(cc[14], cc[0], cc[15], 32) == RT_OK);
  CHECK(bvalue(&c[15]) == 0);
  rt_mkbool(cc[14], 1);
  CHECK(rt_lt(cc[0], cc[14], cc[15], 33) == RT_ERR); /* number < bool */
  rt_err_clear();

  /* concat: strings and numbers (cells must be contiguous) */
  {
    TValue pair[2];
    rt_addr pc[2] = { cell_of(&pair[0]), cell_of(&pair[1]) };
    rt_intern(pc[0], (rt_addr)(uintptr_t)"x=", 2);
    rt_mknumber(pc[1], 3);
    int cstat = rt_concat(pc[0], 2, cc[13], 41);
    CHECK(cstat == RT_OK);
    CHECK(ttisstring(&c[13]) && strcmp(svalue(&c[13]), "x=3") == 0);
  }

  /* forprep: coercion + error message parity */
  {
    TValue fv[3];
    rt_addr fc[3] = { cell_of(&fv[0]), cell_of(&fv[1]), cell_of(&fv[2]) };
    rt_intern(fc[0], (rt_addr)(uintptr_t)"3", 1);
    rt_mknumber(fc[1], 10);
    rt_mknumber(fc[2], 1);
    CHECK(rt_forprep(fc[0], 50) == RT_OK);
    CHECK(nvalue(&fv[0]) == 3);
    rt_mkbool(fc[2], 1);
    CHECK(rt_forprep(fc[0], 51) == RT_ERR);
    rt_err_clear();
  }

  /* error staging: bytes + clear */
  {
    char buf[256];
    rt_mknil(cc[0]);
    rt_mknil(cc[1]);
    CHECK(rt_gettable(cc[0], cc[1], cc[2], 77) == RT_ERR);
    int32_t n = rt_err_stage_copy((rt_addr)(uintptr_t)buf, sizeof buf);
    CHECK(n > 0);
    CHECK(strncmp(buf, "script:77:", 10) == 0);
    CHECK(rt_err_pending() == 1);
    rt_err_clear();
    CHECK(rt_err_stage_copy((rt_addr)(uintptr_t)buf, sizeof buf) == 0);
  }

  /* calls: a Lua closure through the ABI (the lvm-fallback path) */
  {
    const char *src = "local function add(a,b) return a+b end return add";
    lua_State *T = L;
    CHECK(luaL_loadbuffer(T, src, strlen(src), "=test") == 0);
    lua_call(T, 0, 1); /* add on the stack */
    /* move the closure value off the stack into a local cell */
    TValue fcell, args[2];
    fcell = *(T->top - 1);
    /* keep the stack reference alive across the call: a C-stack TValue is
       invisible to the GC, and the ABI cell alone could let the closure
       be collected mid-call. The backend owes every callable the same
       liveness while its cell is live. Args must be a real array — the
       ABI treats argcells as contiguous slots (a lesson this test
       learned the hard way: two separate locals are not adjacent). */
    rt_mknumber(cell_of(&args[0]), 20);
    rt_mknumber(cell_of(&args[1]), 22);
    int32_t st = rt_call(cell_of(&fcell), cell_of(&args[0]), 2, 1, 60);
    T->top--; /* release the kept reference */
    CHECK(st == RT_OK);
    CHECK(nvalue(&args[0]) == 42); /* result overwrites the first arg cell */
  }

  /* ABI v3: the rt-owned open-upvalue registry — open → write-through →
     close → snapshot, and close-level ordering */
  {
    TValue cells[4];  /* a fake wasm frame: four contiguous register cells */
    TValue c0, c1, out;
    rt_addr cf = cell_of(&cells[0]);
    CHECK(rt_wasm_proto(0, 0, 0, 1, 8) == RT_OK);
    rt_wasm_upval(0, 0, 1, 1); /* the closure captures register 1 */
    rt_mknumber(cell_of(&cells[1]), 42);
    CHECK(rt_newclosure(cell_of(&c0), 0, 0, cf, 1) == RT_OK);
    CHECK(ttisfunction(&c0));
    CHECK(rt_clidx(cell_of(&c0)) == 0);
    rt_mknumber(cell_of(&c1), 1);
    CHECK(rt_clidx(cell_of(&c1)) == -1);
    /* open: GETUPVAL reads the frame cell */
    rt_mknil(cell_of(&out));
    rt_getupval((rt_addr)(uintptr_t)clvalue(&c0), 0, cell_of(&out));
    CHECK(ttisnumber(&out) && nvalue(&out) == 42);
    /* write-through: SETUPVAL lands in the open frame cell */
    rt_mknumber(cell_of(&out), 99);
    rt_setupval((rt_addr)(uintptr_t)clvalue(&c0), 0, cell_of(&out));
    CHECK(nvalue(&cells[1]) == 99);
    /* close-level ordering: closing ABOVE the cell leaves it open */
    rt_close_upvals(cf + 2 * (rt_addr)sizeof(TValue));
    rt_mknumber(cell_of(&cells[1]), 77);
    rt_mknil(cell_of(&out));
    rt_getupval((rt_addr)(uintptr_t)clvalue(&c0), 0, cell_of(&out));
    CHECK(nvalue(&out) == 77);
    /* close AT the frame: the value is snapshotted; later frame writes
       are invisible through the closure */
    rt_close_upvals(cf);
    rt_mknumber(cell_of(&cells[1]), 111);
    rt_mknil(cell_of(&out));
    rt_getupval((rt_addr)(uintptr_t)clvalue(&c0), 0, cell_of(&out));
    CHECK(nvalue(&out) == 77);
    /* two closures, two levels: closing the higher cell leaves the lower
       one open */
    CHECK(rt_wasm_proto(1, 0, 0, 1, 8) == RT_OK);
    rt_wasm_upval(1, 0, 1, 0); /* captures register 0 */
    rt_mknumber(cell_of(&cells[0]), 5);
    CHECK(rt_newclosure(cell_of(&c1), 1, 0, cf, 1) == RT_OK);
    rt_close_upvals(cf + (rt_addr)sizeof(TValue)); /* closes cells[1] only */
    rt_mknumber(cell_of(&cells[1]), 0);
    rt_mknil(cell_of(&out));
    rt_getupval((rt_addr)(uintptr_t)clvalue(&c1), 0, cell_of(&out));
    CHECK(nvalue(&out) == 5); /* cells[0] still open, reads through */
  }

  if (failures == 0) printf("RT-NATIVE PASS\n");
  else printf("RT-NATIVE FAIL (%d)\n", failures);
  return failures;
}

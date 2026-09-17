/*
** $Id: linit.c,v 1.14.1.1 2007/12/27 13:02:25 roberto Exp $
** Initialization of libraries for lua.c
** See Copyright Notice in lua.h
*/


#define linit_c
#define LUA_LIB

#include "lua.h"

#include "lualib.h"
#include "lauxlib.h"


static const luaL_Reg lualibs[] = {
  {"", luaopen_base},
#ifndef LUAWASM_PROD
  /* M6c: the production flavor opens base/table/string/math only. Gating
     package/io/os/debug out is what strips every wasi_snapshot_preview1
     import from the linked blob (fs/env/clock all enter through them);
     the sandbox globals list itself is applied by rt_sandbox (rt_abi.c). */
  {LUA_LOADLIBNAME, luaopen_package},
#endif
  {LUA_TABLIBNAME, luaopen_table},
#ifndef LUAWASM_PROD
  {LUA_IOLIBNAME, luaopen_io},
  {LUA_OSLIBNAME, luaopen_os},
#endif
  {LUA_STRLIBNAME, luaopen_string},
  {LUA_MATHLIBNAME, luaopen_math},
#ifndef LUAWASM_PROD
  {LUA_DBLIBNAME, luaopen_debug},
#endif
  {NULL, NULL}
};


LUALIB_API void luaL_openlibs (lua_State *L) {
  const luaL_Reg *lib = lualibs;
  for (; lib->func; lib++) {
    lua_pushcfunction(L, lib->func);
    lua_pushstring(L, lib->name);
    lua_call(L, 1, 0);
  }
}


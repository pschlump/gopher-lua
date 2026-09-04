/*
** runtime/shim/setjmp.h — replaces wasi-libc's <setjmp.h>, which errors
** out without the (non-standardized) wasm EH proposal. The actual
** implementation is runtime/setjmp_asyncify.c: setjmp/longjmp built on
** Binaryen's Asyncify unwind/rewind, which runs on any MVP wasm engine
** (wazero included). This header is found first via -Iruntime/shim.
*/
#ifndef _LUAWASM_SETJMP_H
#define _LUAWASM_SETJMP_H

typedef unsigned long luawasm_jmp_buf[64];

typedef luawasm_jmp_buf jmp_buf;

/* Interpose by macro: the compiler and the Asyncify pass give the literal
   names setjmp/longjmp special (returns_twice / library) treatment that
   breaks the asyncify instrumentation of callers; the renamed functions
   have the real semantics. */
int luawasm_setjmp(jmp_buf env) __attribute__((returns_twice));
void luawasm_longjmp(jmp_buf env, int value) __attribute__((noreturn));

#define setjmp(env) luawasm_setjmp(env)
#define longjmp(env, val) luawasm_longjmp(env, val)

#endif /* _LUAWASM_SETJMP_H */

/*
** runtime/selftest.c — standalone validation of the Asyncify setjmp/
** longjmp machinery (runtime/setjmp_asyncify.c) through the exact build
** pipeline used for the Lua oracle (clang reactor + wasm-opt --asyncify).
** Go gate: runtime/selftest_test.go (skips when the .wasm is not built).
*/

#include <stdint.h>
#include <stdio.h>

#include "shim/setjmp.h"
#include "setjmp_impl.h"

static int failures = 0;
#define CHECK(cond) do { \
	if (!(cond)) { failures++; printf("FAIL %s:%d: %s\n", __FILE__, __LINE__, #cond); } \
} while (0)

static jmp_buf jb_basic, jb_outer, jb_inner;

/* test 1: basic longjmp with a value, several frames deep */
static void t3(void) { longjmp(jb_basic, 7); }
static void t2(void) { t3(); CHECK(0); }
static void t1(void) { t2(); CHECK(0); }

/* test 2: longjmp over an inner protected frame to an outer one */
static int inner_entered = 0;
static void deep(void) {
	if (setjmp(jb_inner) == 0) {
		inner_entered = 1;
		longjmp(jb_outer, 5); /* jumps over jb_inner's frame */
	}
	CHECK(0 && "inner setjmp resumed unexpectedly");
}

static void run_tests(void *unused) {
	(void)unused;
	setvbuf(stdout, NULL, _IONBF, 0);
	
		/* 1 */
	int r = setjmp(jb_basic);
	if (r == 0) t1();
	else CHECK(r == 7);

		/* 2 */
	int r2 = setjmp(jb_outer);
	if (r2 == 0) deep();
	else CHECK(r2 == 5);
	CHECK(inner_entered);

		/* 3: repeated setjmp at the same stack address: fresh record each
	   iteration, one iteration delivers a longjmp */
	int count = 0;
	for (int i = 0; i < 3; i++) {
		jmp_buf loop; /* same address every iteration */
		int r3 = setjmp(loop);
		if (r3 == 0) {
			count++;
			if (i == 1) longjmp(loop, 9);
		} else {
			CHECK(r3 == 9);
		}
	}
	CHECK(count == 3);

		/* 4: longjmp with value 0 must arrive as 1 */
	int r4 = setjmp(jb_basic);
	if (r4 == 0) longjmp(jb_basic, 0);
	else CHECK(r4 == 1);

		/* 5: value written before the longjmp survives the trip (globals and
	   computed data must be intact after rewind) */
	static volatile int marker = 0;
	int r5 = setjmp(jb_basic);
	if (r5 == 0) {
		marker = 42;
		longjmp(jb_basic, 3);
	} else {
		CHECK(r5 == 3);
		CHECK(marker == 42);
	}

	if (failures == 0) printf("SELFTEST PASS\n");
	else printf("SELFTEST FAIL (%d)\n", failures);
	fflush(stdout);
}

int32_t selftest(void) {
	ASYNC_DRIVER_RUN(run_tests, 0);
	return failures;
}

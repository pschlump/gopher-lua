/*
** runtime/setjmp_asyncify.c — setjmp/longjmp for wasm32-wasi without the
** (non-standardized) wasm EH proposal, built on Binaryen's Asyncify pass.
** Runs on any core-wasm engine, wazero included.
**
** Generalizes kripken/talks jmp.c (validated through this exact pipeline
** by runtime/canon.c) for the nested-protected-call pattern stock Lua
** uses: a stack of live luaD_rawrunprotected frames; a longjmp may target
** any of them from a deeper frame.
**
**   - setjmp records the call chain into its buffer by unwinding to the
**     driver; the driver notes the recorded point and rewinds; setjmp
**     re-enters at function top and returns 0.
**   - longjmp unwinds on a scratch buffer; the driver restores the target
**     setjmp's recorded point and rewinds it; setjmp re-enters and returns
**     the longjmp value.
**   - Fresh invocation vs resume is distinguished by in_flight: a resume
**     is the same logical invocation (in_flight still set); a later call
**     at the same jmp_buf address has in_flight cleared and records anew.
**
** States come from a static pool — no malloc anywhere on this path.
** All unwinding entry points must run inside ASYNC_DRIVER_RUN.
*/

#include <stddef.h>
#include <stdint.h>

void asyncify_start_unwind(void *buf) __attribute__((import_module("asyncify"), import_name("start_unwind")));
void asyncify_stop_unwind(void) __attribute__((import_module("asyncify"), import_name("stop_unwind")));
void asyncify_start_rewind(void *buf) __attribute__((import_module("asyncify"), import_name("start_rewind")));
void asyncify_stop_rewind(void) __attribute__((import_module("asyncify"), import_name("stop_rewind")));

#define ASYNC_SCRATCH (512 * 1024)

/* diagnostics (read via exports; printf is unsafe in asyncified code) */
int sj_diag_setjmp_calls, sj_diag_unwinds, sj_diag_rewinds;
#define SJ_POOL 32

struct async_buf {
	void *top;
	void *end;
	void *unwound;
	char buffer[ASYNC_SCRATCH];
};

static void ab_init(struct async_buf *b) {
	b->top = &b->buffer[0];
	b->end = &b->buffer[ASYNC_SCRATCH];
}

static void ab_note_unwound(struct async_buf *b) { b->unwound = b->top; }
static void ab_restore(struct async_buf *b) { b->top = b->unwound; }

struct sj_state {
	struct sj_state *prev;
	struct async_buf ab;      /* recorded setjmp point; rewound twice */
	struct async_buf deliver; /* scratch for longjmp's unwind */
	int value;
	int in_flight;
	int active;
};

static struct sj_state sj_pool[SJ_POOL];

/* jmp_buf slot usage: [0] slot index + 1 (0 = never stamped). */
struct sj_state *sj_stack; /* head = innermost live setjmp */
static struct sj_state *pending;
static int pending_is_longjmp;

static struct sj_state *state_of(void *env) {
	uint32_t *slots = env;
	uint32_t idx = slots[0];
	if (idx == 0 || idx > SJ_POOL) return NULL;
	return &sj_pool[idx - 1];
}

static struct sj_state *state_arm(void *env) {
	uint32_t *slots = env;
	for (int i = 0; i < SJ_POOL; i++) {
		if (!sj_pool[i].active) {
			sj_pool[i].active = 1;
			sj_pool[i].in_flight = 0;
			sj_pool[i].value = 0;
			sj_pool[i].prev = NULL;
			slots[0] = (uint32_t)(i + 1);
			return &sj_pool[i];
		}
	}
	__builtin_trap(); /* protected-call nesting exceeded SJ_POOL */
}

__attribute__((noinline, returns_twice))
int luawasm_setjmp(void *env) {
	struct sj_state *s = state_of(env);
	if (s != NULL && s->in_flight) {
		/* resume: re-entered at function top after a rewind. This is the
		   immediate record-rewind (value==0) or a longjmp delivery. */
		asyncify_stop_rewind();
		s->in_flight = 0;
		if (s->value != 0) {
			/* delivered by longjmp: this protected frame is dead */
			sj_stack = s->prev;
		}
		return s->value;
	}
	/* fresh invocation: record the stack by unwinding to the driver.
	   Nothing but a return may follow start_unwind — during the unwind
		   the function must return immediately (stop_rewind here would be
		   an illegal state transition). */
	if (s == NULL) s = state_arm(env);
	s->in_flight = 1;
	s->value = 0;
	s->prev = sj_stack;
	sj_stack = s;
	pending = s;
	pending_is_longjmp = 0;
	ab_init(&s->ab);
	sj_diag_setjmp_calls++;
	asyncify_start_unwind(&s->ab);
	sj_diag_unwinds++;
	return s->value; /* not reached by the caller: the unwind is in flight */
}

__attribute__((noinline, noreturn))
void luawasm_longjmp(void *env, int value) {
	struct sj_state *s = state_of(env);
	if (s == NULL) __builtin_trap(); /* no protecting frame: fatal */
	s->value = (value == 0) ? 1 : value;
	pending = s;
	pending_is_longjmp = 1;
	ab_init(&s->deliver);
	asyncify_start_unwind(&s->deliver);
	__builtin_unreachable();
}

/* ---- driver support ----
**
** The loop itself is ASYNC_DRIVER_RUN at the call site (setjmp_impl.h):
** the asyncify rewind intrinsics must fire in the frame that called the
** body (dispatching them through a helper function breaks the replay;
** canon3.c demonstrates the failure). These are the trivial leaf
** accessors the macro uses. */

int sj_diag_setjmp_calls, sj_diag_unwinds, sj_diag_rewinds;

struct sj_state *sj_take_pending(int *is_longjmp) {
	struct sj_state *s = pending;
	if (s != NULL) {
		pending = NULL;
		*is_longjmp = pending_is_longjmp;
	}
	return s;
}

void *sj_abuf(struct sj_state *s) { return &s->ab; }
void sj_note_unwound(struct sj_state *s) { s->ab.unwound = s->ab.top; }
void sj_restore(struct sj_state *s) { s->ab.top = s->ab.unwound; }


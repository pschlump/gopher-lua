/* Declarations shared by setjmp_asyncify.c and its users.
**
** ASYNC_DRIVER_RUN must be used at every export that can reach setjmp.
** The asyncify_* intrinsics that drive the unwind/rewind cycle MUST be
** called from the same frame that called the body — dispatching them
** through a helper function breaks the replay (canon3.c demonstrates the
** failure) — so the loop is a macro and only trivial leaf accessors
** (sj_take_pending/sj_abuf/sj_note_unwound/sj_restore) are functions. */
#ifndef SETJMP_IMPL_H
#define SETJMP_IMPL_H

void asyncify_start_unwind(void *buf) __attribute__((import_module("asyncify"), import_name("start_unwind")));
void asyncify_stop_unwind(void) __attribute__((import_module("asyncify"), import_name("stop_unwind")));
void asyncify_start_rewind(void *buf) __attribute__((import_module("asyncify"), import_name("start_rewind")));
void asyncify_stop_rewind(void) __attribute__((import_module("asyncify"), import_name("stop_rewind")));

struct sj_state;

/* consumes the pending unwind target; NULL when the body finished
   normally. *is_longjmp distinguishes longjmp delivery from a setjmp
   record. */
struct sj_state *sj_take_pending(int *is_longjmp);

/* the asyncify buffer of a state (for start_rewind at the call site) */
void *sj_abuf(struct sj_state *s);
void sj_note_unwound(struct sj_state *s);
void sj_restore(struct sj_state *s);

#define ASYNC_DRIVER_RUN(bodyfn, argval) do { \
	void *_ad_arg = (argval); \
	(void)_ad_arg; \
	for (;;) { \
		bodyfn(_ad_arg); \
		int _ad_islj = 0; \
		struct sj_state *_ad_s = sj_take_pending(&_ad_islj); \
		if (_ad_s == NULL) break; \
		asyncify_stop_unwind(); \
		if (_ad_islj) sj_restore(_ad_s); \
		else sj_note_unwound(_ad_s); \
		asyncify_start_rewind(sj_abuf(_ad_s)); \
	} \
} while (0)

#endif

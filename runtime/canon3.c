/*
** runtime/canon.c — kripken/talks jmp.c reduced to a reactor export, run
** through the same pipeline as the Lua oracle (wasi-sdk clang reactor +
** wasm-opt --asyncify) to validate the pipeline itself on wazero.
*/
#include <stdint.h>
#include <stdio.h>

#define NOINLINE __attribute__((noinline))

/* The Asyncify API */
void asyncify_start_unwind(void *buf) __attribute__((import_module("asyncify"), import_name("start_unwind")));
void asyncify_stop_unwind(void) __attribute__((import_module("asyncify"), import_name("stop_unwind")));
void asyncify_start_rewind(void *buf) __attribute__((import_module("asyncify"), import_name("start_rewind")));
void asyncify_stop_rewind(void) __attribute__((import_module("asyncify"), import_name("stop_rewind")));

#define ASYNC_BUF_BUFFER_SIZE 100000

struct async_buf {
  void* top;
  void* end;
  void* unwound;
  char buffer[ASYNC_BUF_BUFFER_SIZE];
};

NOINLINE void async_buf_init(struct async_buf* buf) {
  buf->top = &buf->buffer[0];
  buf->end = &buf->buffer[ASYNC_BUF_BUFFER_SIZE];
}
NOINLINE void async_buf_note_unwound(struct async_buf* buf) { buf->unwound = buf->top; }
NOINLINE void async_buf_rewind(struct async_buf* buf) { buf->top = buf->unwound; }

struct jmp_buf_canon {
  struct async_buf setjmp_buf;
  struct async_buf longjmp_buf;
  int value;
  int state;
};

static struct jmp_buf_canon* __active_jmp_buf = NULL;

NOINLINE int setjmp_canon(struct jmp_buf_canon* buf) {
  if (buf->state == 0) {
    __active_jmp_buf = buf;
    async_buf_init(&buf->setjmp_buf);
    asyncify_start_unwind(&buf->setjmp_buf);
  } else {
    asyncify_stop_rewind();
    if (buf->state == 2) {
      __active_jmp_buf = NULL;
    }
  }
  buf->state++;
  return buf->value;
}

NOINLINE void longjmp_canon(struct jmp_buf_canon* buf, int value) {
  buf->value = value;
  async_buf_init(&buf->longjmp_buf);
  asyncify_start_unwind(&buf->longjmp_buf);
}

/* the user program */
struct jmp_buf_canon my_buf;
int canon_rc = 0;

NOINLINE void inner() {
  canon_rc += 2;
  longjmp_canon(&my_buf, 1);
}

NOINLINE void user_program() {
  canon_rc += 1;              /* start */
  if (!setjmp_canon(&my_buf)) {
    canon_rc += 4;            /* call-inner path */
    inner();
  } else {
    canon_rc += 8;            /* back-from-longjmp path */
  }
  canon_rc += 16;             /* end */
}

__attribute__((noinline))
static void my_dispatch(struct jmp_buf_canon *s, int is_longjmp) {
  asyncify_stop_unwind();
  if (is_longjmp) {
    async_buf_rewind(&s->setjmp_buf);
  } else {
    async_buf_note_unwound(&s->setjmp_buf);
  }
  asyncify_start_rewind(&s->setjmp_buf);
}

int32_t canon3(void) {
  for (;;) {
    user_program();
    if (!__active_jmp_buf) {
      return canon_rc; /* expect 31 */
    }
    my_dispatch(__active_jmp_buf, __active_jmp_buf->state == 2);
  }
}

int32_t canon() {
  while (1) {
    user_program();
    if (!__active_jmp_buf) {
      return canon_rc; /* expect 1+4+2+8+16 = 31 */
    }
    asyncify_stop_unwind();
    if (__active_jmp_buf->state == 1) {
      async_buf_note_unwound(&__active_jmp_buf->setjmp_buf);
    } else if (__active_jmp_buf->state == 2) {
      async_buf_rewind(&__active_jmp_buf->setjmp_buf);
    }
    asyncify_start_rewind(&__active_jmp_buf->setjmp_buf);
  }
}

/* M3 boundary experiment: exports memory + a reader, so an emitted module
   can share this address space and pass pointers to functions here. */
#include <stdint.h>

int32_t rt_peek(int32_t addr) {
  return *(int32_t *)(size_t)addr;
}

void rt_poke(int32_t addr, int32_t v) {
  *(int32_t *)(size_t)addr = v;
}

int32_t rt_add_at(int32_t a, int32_t b) {
  return *(int32_t *)(size_t)a + *(int32_t *)(size_t)b;
}

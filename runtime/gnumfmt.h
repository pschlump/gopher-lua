#ifndef GN_NUMFMT_H
#define GN_NUMFMT_H

/* fork-exact number→string (gnumfmt.c). buf must hold ≥ 26 bytes. */
void gn_lua_number_to_string(char *buf, double v);

/* strconv.FormatFloat(v, 'g', -1, 64) for finite v (NaN/Inf included for
** completeness). Returns bytes written excl. NUL; dst ≥ 26 bytes. */
int gn_fmt_g_shortest(char *dst, double v);

#endif

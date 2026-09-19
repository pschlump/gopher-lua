-- Row 38 fix (2026-09-19): boundary values of the fork's number
-- rendering, ported into runtime/gnumfmt.c (Go-1.27 strconv shortest-'g'
-- + LNumber.String()'s integer fast path with arm64 saturating
-- float64→int64). Cross-checked against the interp oracle over 7.9M
-- values (random bit patterns + binade/decimal boundaries) — zero
-- mismatches. Note tostring(-0.0) is "0" (the fork's int64(-0.0) drops
-- the sign — row 42/38 corner) and 2^63 prints SATURATED (int64(2^63)
-- = MaxInt64 on arm64, and float64(MaxInt64) == 2^63 so it passes
-- isInteger). math.huge stays OUT of this pin: the fork's value is
-- finite MaxFloat64, C's is HUGE_VAL — a value divergence still ruled
-- by row 38, not a formatting one.
print(tostring(1e15), tostring(1e16), tostring(1234567.5), tostring(999999.5))
print(tostring(1e-5), tostring(1e-7), tostring(5e-324), tostring(1e308))
print(tostring(1 / 3), tostring(-(1 / 3)), tostring(-2.5), tostring(0.1))
print(tostring(-0.0), tostring(0.0), tostring(2^53), tostring(2^53 + 2))
print(tostring(9223372036854775808.0), tostring(-9223372036854775808.0)) -- 2^63 sat, -2^63
print(tostring(9.3e18), tostring(-9.3e18)) -- beyond ±2^63: never integer → %g form
print(tostring(1e308 * 10), tostring(-(1e308 * 10)))                     -- +Inf / -Inf
print(tostring(0 / 0))                                                    -- NaN
print((1.5) .. "s", (0 - 0.0) .. "s", (5e-324) .. "s")
print(string.format("%d %s", 42, tostring(1234567.5)))

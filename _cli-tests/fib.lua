-- fib.lua — Fibonacci sequences. Usage: fib.lua [n]
-- Prints the first min(n, 46) Fibonacci numbers in full (46 is the last
-- term below 2^31 — %d's wasm32 wall in the C engines), then fib(n)
-- modulo 1000000007 — the competitive-programming habit — which stays
-- exact for any n.

local n = tonumber(arg[1]) or 10
if n < 0 then
    n = 0
end

local a, b = 0, 1
local parts = {}
for i = 1, math.min(n, 46) do
    parts[#parts + 1] = string.format("%d", a)
    a, b = b, a + b
end
print(table.concat(parts, " "))

local m = 1000000007
local x, y = 0, 1
for i = 1, n do
    x, y = y, (x + y) % m
end
print(string.format("fib(%d) mod 1000000007 = %d", n, x))

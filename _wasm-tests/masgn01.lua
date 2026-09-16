-- row 30: simultaneous multiple assignment to locals (regression
-- coverage for the compile.go fix — interp and wasm share the frontend,
-- so this pins the backend's lowering of the temp-window + MOVE shape).
local a, b = 0, 1
a, b = b, a + b
print(a, b)
local x, y = 1, 2
x, y = y, x
print(x, y)
local p, q, r = 1, 2, 3
p, q, r = r, p, q
print(p, q, r)
local c = 0
c, c = 1, 2
print(c)
local u, v = 5, 9
u, v = v - u, u * 2
print(u, v)
local t = {}
local i = 1
t[i], i = "set", i + 1
print(t[1], i)
local m, n = 1, 2
m, n = n
print(m, n)

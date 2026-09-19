-- M6e soak finds (row 38 fix, 2026-09-19): computed values reaching
-- tostring()/concat/table.concat rendered through the stock C "%.14g"
-- while the fork prints integer-valued floats as full integers and
-- non-integers Go-shortest. Findings c0071344/c0750767/c0916336/
-- c1034921/c1066545 — all through tostring of an ARITHMETIC result
-- (the literal pool was already restricted; computed table slots and
-- locals were the leak). The runtime now converts numbers exactly
-- like LNumber.String() in the gopher dialect (runtime/gnumfmt.c).
local t = {}
t[10] = math.max(3, 1e15) + math.abs(100)      -- c0071344: 1e15+100
print(tostring(t[10]), tostring(t[10]))
local a = 3.5 / 3                               -- c0750767
print(tostring(a), ("x" .. (3.5 / 3)))
local b = 1e-9 + 9007199254740993               -- c0916336: rounds to 2^53
print(tostring(b))
local c = 2 / 255                               -- c1034921
print(tostring(c))
local d = math.floor(9007199254740993)          -- c1066545: 2^53
print(tostring(d), tostring(d) == tostring(b))
print(table.concat({1e15 + 100, 3.5 / 3, 2 / 255}, "|"))
print((1e15 + 100) .. "" .. (2 / 255))

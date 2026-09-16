-- row 10u regression: table.sort through the callback machinery —
-- big arrays both directions, an error-raising comparator unwinding
-- through auxsort's C frame, __lt metamethod ordering, and a comparator
-- that calls compiled code. All were broken by call_body's dangling
-- base (sort grows the Lua stack across its nested luaD_calls).
local t = {}
for i = 1, 300 do t[i] = (i * 7919) % 997 end
table.sort(t)
local ok = true
for i = 2, #t do if t[i-1] > t[i] then ok = false end end
print("asc300", ok, t[1], t[300])
table.sort(t, function(a, b) return a > b end)
ok = true
for i = 2, #t do if t[i-1] < t[i] then ok = false end end
print("desc300", ok, t[1], t[300])
local u = {}
for i = 1, 100 do u[i] = "k" .. ((i * 37) % 101) end
table.sort(u)
print("strings", u[1], u[100])
-- comparator raising an error: must unwind through auxsort's C frame
local e = {3, 1, 2}
print(pcall(table.sort, e, function(a, b) error("cmp boom") end))
-- __lt metamethod as the default ordering
local mt = {__lt = function(a, b) return a.v < b.v end}
local objs = {}
for i = 1, 50 do objs[i] = setmetatable({v = (i * 29) % 53}, mt) end
table.sort(objs)
local asc = true
for i = 2, #objs do if objs[i-1].v > objs[i].v then asc = false end end
print("metalt", asc, objs[1].v, objs[50].v)
-- comparator that calls back into compiled code (deep nesting)
local function cmp(a, b)
    return tonumber(a) < tonumber(b)
end
local d = {}
for i = 1, 100 do d[i] = (i * 13) % 61 end
table.sort(d, cmp)
print("cbcomp", d[1], d[100])
print("survived")

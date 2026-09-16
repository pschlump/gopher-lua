-- tsort.lua — table.sort end-to-end (row 10u regression: sort through
-- the callback machinery). Usage: tsort.lua [n]
-- Exercises plain and comparator sorts at scale, an error-raising
-- comparator unwinding through pcall, __lt metamethod ordering, and a
-- comparator that calls compiled code.

local n = tonumber(arg[1]) or 300

local t = {}
for i = 1, n do
    t[i] = (i * 7919) % 997
end
table.sort(t)
local ok = true
for i = 2, #t do
    if t[i-1] > t[i] then ok = false end
end
print(string.format("asc %d ok=%s first=%d last=%d", n, tostring(ok), t[1], t[n]))

table.sort(t, function(a, b) return a > b end)
ok = true
for i = 2, #t do
    if t[i-1] < t[i] then ok = false end
end
print(string.format("desc %d ok=%s first=%d last=%d", n, tostring(ok), t[1], t[n]))

local u = {}
for i = 1, 100 do
    u[i] = "k" .. ((i * 37) % 101)
end
table.sort(u)
print("strings", u[1], u[100])

local e = {3, 1, 2}
local ok2, err2 = pcall(table.sort, e, function(a, b) error("cmp boom") end)
-- strip the position prefix: clua-run's arg prelude shifts line numbers
print(ok2, (string.gsub(err2, "^.-:%d+: ", "")))

local mt = {__lt = function(a, b) return a.v < b.v end}
local objs = {}
for i = 1, 50 do
    objs[i] = setmetatable({v = (i * 29) % 53}, mt)
end
table.sort(objs)
local asc = true
for i = 2, #objs do
    if objs[i-1].v > objs[i].v then asc = false end
end
print("metalt", tostring(asc), objs[1].v, objs[50].v)

local function cmp(a, b)
    return tonumber(a) < tonumber(b)
end
local d = {}
for i = 1, 100 do
    d[i] = (i * 13) % 61
end
table.sort(d, cmp)
print("cbcomp", d[1], d[100])
print("survived")

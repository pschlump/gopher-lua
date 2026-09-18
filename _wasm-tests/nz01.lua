-- M6e fuzz find (row 42) layout flavor, FIXED (see nz00): the
-- minimized program keeps its odd shape (globals, -(0), metamethod
-- set) verbatim because that exact layout was load-bearing for the
-- original -0 divergence.
local t1 = {"10", false, "hello world", ""}
print(v1, v3, (-(0)))
for kk0, vv0 in ipairs(t1) do
  local mt0 = { __unm = function(a) return -7 end, __add = function(a, b) return 100 end, __eq = function(a, b) return true end, __call = function(s, x) return x * 2 end, __concat = function(a, b) return "cc" end }
  local t3 = setmetatable({"A", "A"}, mt0)
  print((-t3), (t3 + t4), (t3 == t4), t3(0), ("lua" .. t3))
end
print(v1, v3, (-(0)))
for kk0, vv0 in ipairs(t1) do
  local mt0 = { __unm = function(a) return -7 end, __add = function(a, b) return 100 end, __eq = function(a, b) return true end, __call = function(s, x) return x * 2 end, __concat = function(a, b) return "cc" end }
  local t3 = setmetatable({"A", "A"}, mt0)
  print((-t3), (t3 + t4), (t3 == t4), t3(0), ("lua" .. t3))
end

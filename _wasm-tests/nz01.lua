-- M6e fuzz find (row 42), layout flavor: __call(0) with x*2 gives -0
-- on wasm (kcell misread), 0 on interp — here triggered by the -(0)
-- constant and surrounding pool layout.
local t1 = {"10", false, "hello world", ""}
print(v1, v3, (-(0)))
for kk0, vv0 in ipairs(t1) do
  local mt0 = { __unm = function(a) return -7 end, __add = function(a, b) return 100 end, __eq = function(a, b) return true end, __call = function(s, x) return x * 2 end, __concat = function(a, b) return "cc" end }
  local t3 = setmetatable({"A", "A"}, mt0)
  print((-t3), (t3 + t4), (t3 == t4), t3(0), ("lua" .. t3))
end

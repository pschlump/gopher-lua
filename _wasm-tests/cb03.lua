local mt = {__index = function(t, k) return k .. '!' end}
local t = setmetatable({}, mt)
print(t.foo, t.bar)

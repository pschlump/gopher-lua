local mt = {__index = function(t, k) error('index boom') end}
local t = setmetatable({}, mt)
print(t.foo)

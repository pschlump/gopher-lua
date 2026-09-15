local mt = {__index = function(t, k) error('idx') end}
local t = setmetatable({}, mt)
print(pcall(function() return t.foo end))

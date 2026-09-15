local mt = {__add = function(a, b) error('add boom') end}
local t = setmetatable({}, mt)
print(t + 1)

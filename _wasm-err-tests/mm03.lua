local mt = {__lt = function(a, b) error('lt boom') end}
local t = setmetatable({}, mt)
print(t < t)

local mt = {__concat = function(a, b) error('cc boom') end}
local t = setmetatable({}, mt)
print(t .. 'x')

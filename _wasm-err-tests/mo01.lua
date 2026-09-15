local n = 0
local mt = {__add = function(a, b) n = n + 1 return n end}
local t = setmetatable({}, mt)
local _ = t + 1
local _ = t + 1
print(n)

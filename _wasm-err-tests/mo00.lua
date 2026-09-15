local n = 0
local mt = {__index = function(t, k) n = n + 1 return n end}
local t = setmetatable({}, mt)
t.a t.a t.a
print(n)

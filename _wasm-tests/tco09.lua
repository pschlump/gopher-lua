local mt = {__call = function(self, n) if n == 0 then return 'cd' end return self(n-1) end}
local f = setmetatable({}, mt)
print(f(120))

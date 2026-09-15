local acc = 0
local function f(n) acc = acc + 1 if n == 0 then return acc end return f(n-1) end
print(f(50000))

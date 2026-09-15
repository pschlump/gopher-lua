local function g(a, b) return a + b end
local function f(...) return g(...) end
print(f(3, 4))

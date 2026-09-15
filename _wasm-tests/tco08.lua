local function g(...) return ... end
local function f(...) return g(2, ...) end
print(f(1, 2, 3))

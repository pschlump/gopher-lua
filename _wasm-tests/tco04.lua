local function g(...) return ... end
local function f(...) return g(...) end
print(f(1, 2, 3))

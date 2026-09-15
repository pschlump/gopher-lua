local function h(...) return ... end
local function g(...) return h(...) end
local function f(...) return g(2, ...) end
print(f(1, 2, 3))

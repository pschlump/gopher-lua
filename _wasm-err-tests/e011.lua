local function g() error('lvl2', 2) end
local function f() g() end
print(pcall(f))

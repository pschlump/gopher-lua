local function g() error('m', 1) end
local function f() g() end
f()

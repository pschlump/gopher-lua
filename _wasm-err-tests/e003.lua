local function g() error('m', 2) end
local function f() g() end
f()

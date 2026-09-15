local function g() error('m', 3) end
local function f() g() end
local function h() f() end
h()

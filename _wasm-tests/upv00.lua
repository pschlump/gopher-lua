-- M6e fuzz find (row 44), regression pin: the interp's raiseError used
-- to closeAllUpvalues() — closing SURVIVING frames' upvalues too — so
-- after a caught error the closure's v0=100 wrote a dead copy and the
-- owning frame kept the stale register. wasm matched stock C.
local v0 = -256
local function f0()
  local ok, e = pcall(function() return (5)() end)
  v0 = 100
end
f0()
print(v0)
local function set() v0 = 7 end
set()
print(v0)

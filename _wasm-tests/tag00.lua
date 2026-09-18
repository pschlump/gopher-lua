-- M6e fuzz find (row 45): bools/nils written into RECYCLED frame cells kept
-- garbage in the upper 3 bytes of the 4-byte TValue tt field (the backend
-- stored tags with a 1-byte store). The top-level rt_call cycle frees and
-- re-mallocs the 1 MiB frame chunk after every adapter unwind, so a later
-- closure staged `false` over stale string bytes → pcall printed (true, nil).
-- interp: true false   wasm (bug): true nil
local function f0(a)
  local function f1(a, b)
  end
  print(f1(("" .. -2), a))
end
print(f0(100))
local ok, e1, e2 = pcall(function() return #missing_global_q end)
local ok, e1, e2 = pcall(function() return {} + 1 end)
print(pcall(function() return false end))

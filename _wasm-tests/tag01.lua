-- M6e fuzz find (row 45, second shape): a ≥15-deep recursion crosses the
-- frame-chunk free/re-malloc cycle; the following pairs iteration read
-- t[3]'s bool with a garbage tag → printed nil instead of true.
-- interp: 3 true   wasm (bug): 3 nil
local function f0()
  local t1 = {1, 0.1, true}
  for kk0, vv0 in pairs(t1) do
    print(kk0, vv0)
    local function f1(n)
      if n <= 0 then return 2 end
      return f1(n - 1) + n
    end
    print(f1(7))
  end
end
local r1, r2, r3 = f0()
local function f4(n)
  if n <= 0 then return 10 end
  return f4(n - 1) + n
end
print(f4(50))
local t3 = {"", 255, false}
for kk1, vv1 in pairs(t3) do
  print(f0())
end

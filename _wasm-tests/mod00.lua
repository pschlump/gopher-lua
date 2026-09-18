-- M6e fuzz find (row 43), regression pin: % over RUNTIME operands —
-- stock 5.1's floor-formula luai_nummod double-rounds (3%0.1 → 0)
-- while the fork's luaModulo (fmod+adjust) gives 0.0999… → "0.1".
-- The runtime now mirrors the fork. Literal forms never diverged
-- (the Go frontend folds them with luaModulo at compile time).
local x = math.random(3)
print(x, x % 0.1)
print(math.random(10) % 1e-9)
print(-0.0 % 3.5)
print(-7 % 3, 7 % -3)

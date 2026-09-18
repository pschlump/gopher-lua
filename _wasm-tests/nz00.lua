-- M6e fuzz find (row 42), FIXED: this looked like constant-pool
-- corruption but was two upstream fork bugs — LNumber2I canonicalized
-- every computed -0.0 to +0.0, and ConstIndex merged the 0 literal
-- onto the -0.0 pool slot, so t(0) actually passed -0.0. Interp then
-- printed 0 (canonicalized), wasm -0 (stock C behavior). Both fixed;
-- this pins the agreement.
local t2 = {-0.0, 1, -0.0}
local t = setmetatable({}, { __call = function(s, x) return x * 2 end })
print(t(0))

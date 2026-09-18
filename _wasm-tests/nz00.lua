-- M6e fuzz find (row 42): the mere presence of a -0.0 CONSTANT in the
-- chunk corrupts the constant index a metamethod's arithmetic reads —
-- t(0) with __call returning x*2 gives -0 on wasm (it read the -0.0
-- kcell instead of the 2), 0 on interp. Any -0.0 literal anywhere in
-- the chunk triggers it; computed -0.0 does not.
local t2 = {-0.0, 1, -0.0}
local t = setmetatable({}, { __call = function(s, x) return x * 2 end })
print(t(0))

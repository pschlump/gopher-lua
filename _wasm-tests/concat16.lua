-- row 27: cumulative OP_CONCAT executions in one frame corrupt the run
-- at >= 16 (silent early exit, garbage error bytes, or the nested-
-- dispatch panic); non-monotonic — 15 and 100 iterations pass, 16 dies.
-- No metamethods, no callbacks: plain string concat in a loop.
local t = {}
for i = 1, 16 do
    t[i] = "a" .. "b"
end
print(#t, t[1], t[16])
print("survived")

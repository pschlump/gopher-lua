-- concatstress.lua — the ledger row 27 pinner: >= 16 runtime string
-- concats executed in ONE frame used to corrupt the wasm run (silent
-- truncation, garbage error bytes, or the nested-dispatch panic): each
-- rt_concat leaked one Lua-stack slot (top crept past the frame).
-- Fixed in runtime/rt_abi.c concat_body; this now passes everywhere.

local parts = {}
for i = 1, 40 do
    parts[i] = "#" .. i .. ":" .. string.rep("x", i % 5 + 1)
end
for i = 1, 40 do
    print(parts[i])
end
print("survived")

-- concatstress.lua — the ledger row 27 pinner: >= 16 runtime string
-- concats executed in ONE frame corrupt the wasm run (silent truncation,
-- garbage error bytes, or the nested-dispatch panic). Interp and clua
-- sail through; the Makefile xfail-targets the wasm leg until the row
-- is fixed.

local parts = {}
for i = 1, 40 do
    parts[i] = "#" .. i .. ":" .. string.rep("x", i % 5 + 1)
end
for i = 1, 40 do
    print(parts[i])
end
print("survived")

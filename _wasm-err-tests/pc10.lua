-- row 28: pcall-caught core raises keep the position prefix in interp
-- (and stock C Lua); the wasm path returns the bare message without it.
local ok1, e1 = pcall(function()
    local t = nil
    return t.field
end)
print(ok1, e1)
local ok2, e2 = pcall(function()
    return "a" .. nil
end)
print(ok2, e2)

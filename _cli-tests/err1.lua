-- err1.lua — uncaught-error fidelity at the CLI seam. The pcall-caught
-- messages (with position prefixes) must match interp and wasm byte for
-- byte (M5d dialect); clua-run keeps stock C 5.1 wording plus the
-- one-line arg-prelude shift, so it gets its own golden file. The final
-- uncaught error also pins each runner's top-level error surface
-- (stdout for interp, "glua: <quoted>" on stderr for -W).

local t = nil

print("before the crash")

local ok, err = pcall(function()
    return t.field
end)
print(ok, err)

local ok2, err2 = pcall(function()
    local x = "a" .. nil
end)
print(ok2, err2)

error("deliberate uncaught error")

print("unreachable")

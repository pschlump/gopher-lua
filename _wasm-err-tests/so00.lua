local function f() return 1 + f() end
print(pcall(f))

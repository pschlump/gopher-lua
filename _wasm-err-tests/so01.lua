local function f() local t = {} t[1] = f() return t end
print(pcall(f))

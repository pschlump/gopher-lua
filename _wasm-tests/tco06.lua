local function f(n) if n == 0 then error('deep') end return f(n-1) end
print(pcall(f, 10000))

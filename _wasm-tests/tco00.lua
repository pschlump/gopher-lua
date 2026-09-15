local function f(n) if n == 0 then return 'done' end return f(n-1) end
print(f(100000))

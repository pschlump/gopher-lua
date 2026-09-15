local function apply(f, v) return f(v) end
print(apply(function(x) return x * 3 end, 5))

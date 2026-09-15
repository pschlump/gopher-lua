local function f(...) return select('#', ...) end
print(f(nil, nil))

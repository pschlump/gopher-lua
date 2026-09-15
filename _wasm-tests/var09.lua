local function f(a, ...) return a, select('#', ...) end
print(f(1, 2, 3))

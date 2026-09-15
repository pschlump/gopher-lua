local function f(...) return select(-1, ...) end
print(f('a', 'b', 'c'))

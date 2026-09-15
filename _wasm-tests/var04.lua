local function f(...) local t = {...} return #t, t[1], t[3] end
print(f('x', 'y', 'z'))

local function f(...) local n = 0
for _, v in ipairs({...}) do n = n + v end
return n end
print(f(1, 2, 3))

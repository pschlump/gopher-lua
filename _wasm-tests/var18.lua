local function f(...) return table.concat({...}, '-') end
print(f('a', 'b', 'c'))

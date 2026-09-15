local t = {}
local function get() return t end
get().k = 5
print(t.k)

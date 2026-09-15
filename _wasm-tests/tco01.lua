local odd
local function even(n) if n == 0 then return true end return odd(n-1) end
function odd(n) if n == 0 then return false end return even(n-1) end
print(even(100001), odd(100001))

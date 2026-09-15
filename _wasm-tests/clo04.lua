local function outer()
  local x = 1
  local function inner() x = x + 10 return x end
  inner() inner()
  return x
end
print(outer())

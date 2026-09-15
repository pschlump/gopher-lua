local function a()
  local x = 1
  return function()
    local y = 2
    return function() return x + y end
  end
end
print(a()()())

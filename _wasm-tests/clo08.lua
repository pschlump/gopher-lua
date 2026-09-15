local function make(n)
  return function() return n * 2 end
end
print(make(5)(), make(21)())

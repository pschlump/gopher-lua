local fs = {}
for i = 1, 3 do
  local j = i * 10
  fs[i] = function() return j end
end
print(fs[1](), fs[2](), fs[3]())

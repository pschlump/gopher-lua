local f
for i = 1, 3 do
  f = function() return i end
  if i == 1 then break end
end
print(f())

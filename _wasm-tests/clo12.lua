local fns = {}
for i = 1, 2 do
  for j = 1, 2 do
    fns[#fns + 1] = function() return i * 10 + j end
  end
end
local out = {}
for k, f in ipairs(fns) do out[k] = tostring(f()) end
print(table.concat(out, ' '))

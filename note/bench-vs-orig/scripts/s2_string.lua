-- s2_string: short string manipulation (format / concat / sub / byte).
local parts = {}
for i = 1, 40 do
  parts[#parts + 1] = string.format("k%03d=%d", i, i * i)
end
local joined = table.concat(parts, ",")
local sum = 0
for i = 1, #joined do
  local b = string.byte(joined, i)
  if b >= 48 and b <= 57 then
    sum = sum + (b - 48)
  end
end
local tail = string.sub(joined, -12)
print(joined:len(), sum, tail)

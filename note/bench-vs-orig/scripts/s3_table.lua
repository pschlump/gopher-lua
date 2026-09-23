-- s3_table: short table building / lookup / sort.
local t = {}
for i = 1, 200 do
  t[i] = (i * 37) % 211
end
local seen = {}
local uniq = 0
for i = 1, #t do
  local v = t[i]
  if not seen[v] then
    seen[v] = true
    uniq = uniq + 1
  end
end
table.sort(t)
local med = t[#t / 2]
print(uniq, med, t[1])

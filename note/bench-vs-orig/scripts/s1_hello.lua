-- s1_hello: a handful of statements — the "tiny script" shape (ping/eval-ish).
local x = 0
for i = 1, 10 do
  x = x + i
end
local s = ""
for i = 1, 5 do
  s = s .. tostring(i)
end
print("hello", x, s)

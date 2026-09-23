-- l2_nsieve: sieve of Eratosthenes over a table-as-bitarray, 3 rounds
-- (benchmark-game nsieve shape: table stores + integer arithmetic).
local N = 18
local function sieve(n)
  local size = 2 ^ n
  local t = {}
  local count = 0
  for i = 0, size - 1 do
    t[i] = true
  end
  for i = 2, size - 1 do
    if t[i] then
      count = count + 1
      for j = i * i, size - 1, i do
        t[j] = false
      end
    end
  end
  return count
end
for n = N, N + 2 do
  print(string.format("2^%d %d", n, sieve(n)))
end

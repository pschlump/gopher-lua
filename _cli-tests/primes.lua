-- primes.lua — sieve of Eratosthenes. Usage: primes.lua [limit]
-- Prints all primes below the limit on one line, then a summary.

local n = tonumber(arg[1]) or 100
if n < 2 then
    n = 2
end

local sieve = {}
for i = 2, n do
    sieve[i] = true
end

local i = 2
while i * i <= n do
    if sieve[i] then
        local j = i * i
        while j <= n do
            sieve[j] = false
            j = j + i
        end
    end
    i = i + 1
end

local out = {}
local count, sum = 0, 0
for k = 2, n do
    if sieve[k] then
        count = count + 1
        sum = sum + k
        out[#out + 1] = string.format("%d", k)
    end
end

print(table.concat(out, " "))
print(string.format("%d primes below %d, sum %d", count, n, sum))

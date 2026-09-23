-- l1_fib: recursive fib(30) — call-heavy pure compute (1.6M calls).
local function fib(n)
  if n < 2 then
    return n
  end
  return fib(n - 1) + fib(n - 2)
end
print(fib(30))

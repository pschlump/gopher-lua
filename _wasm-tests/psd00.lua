-- M6e fuzz find (row 46): an unreachable JMP chain (dead `if (nil and "x")`
-- under a constant-false `while`) can point backward INTO an OP_CLOSURE's
-- upvalue-capture pseudo-instruction; the block partitioner then started a
-- basic block ON the pseudo, emitting it as real code — the capture GETUPVAL
-- clobbered the closure register and the call died with
-- "attempt to call a non-function object".
local v1 = "hello world"
local function f0()
  local function f1(a)
    print(6, v1)
  end
  f1(false)
  while (2 <= -1) do
    if (nil and "x") then
      while false do
      end
    end
  end
end
f0()
print("done")

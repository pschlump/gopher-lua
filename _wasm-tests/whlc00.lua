-- M6e fuzz find (row 41): a while-condition of the shape
-- (truthy-constant or X) compiles to a dead TEST whose partner is a
-- long-jump pseudo (OP_NOP sBx); the backend skips the loop body.
-- interp: 1 / after   wasm: after (body never runs)
while ("A" or "hello") do
  print(1)
  break
end
print("after")

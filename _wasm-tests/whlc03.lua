-- M6e fuzz find (row 41), silent-exit flavor: an and/or chain over
-- constants in a NESTED if-condition ends the wasm log early (no
-- ERROR line) — interp prints three lines, wasm stops after "nil".
for i = 20, 3, -1 do
  if "10" then
    print(nil)
    if (("2" and true) or "") then
      print(42)
    end
  end
end
print("after")

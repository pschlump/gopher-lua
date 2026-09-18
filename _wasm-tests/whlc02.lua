-- row 41, return flavor: wasm falls through the loop to print("after")
while ("A" or "hello") do
  return 7
end
print("after")

-- M6e fuzz find (row 47): the INTERP was wrong — parseNumber used Go
-- ParseInt(s, 0), which reads leading-zero decimal strings as OCTAL
-- ("0255" → 173) in arith coercion, while the runtime's stock luaO_str2d
-- (strtod) keeps them decimal (255). tonumber was never affected (its own
-- parser). Fixed by porting luaO_str2d semantics into parseNumber.
print((0 .. 255) + 0, -(0 .. 255), "0255" + 0, "010" * 1)
print(tonumber("0255"), tonumber("010"), "0x10" + 0, -"0x10", tonumber(" 25 "))

-- syntaxerr.lua — a deliberate syntax error: compilation itself fails.
-- The Makefile pins each runner's compile-time error surface (glua -w
-- refuses to emit a module; interp and clua print a parse error).
-- (A bare `local x =` is NOT a syntax error — the next expression
-- completes it — so this stays an unbalanced parenthesis.)

local x = (
print("never parsed")

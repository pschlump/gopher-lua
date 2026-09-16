-- caesar.lua — Caesar-shift the letters of files (ROT13 when the shift
-- is 13). Usage: caesar.lua shift [file ...]
-- Non-letters pass through; the shift may be negative (wraps modulo 26).

local shift = tonumber(arg[1])
if shift == nil then
    print("usage: caesar.lua shift [file ...]")
    return
end
shift = shift % 26

local function enc(text)
    local out = {}
    for i = 1, #text do
        local b = string.byte(text, i)
        if b >= 65 and b <= 90 then -- 'A'..'Z'
            b = (b - 65 + shift) % 26 + 65
        elseif b >= 97 and b <= 122 then -- 'a'..'z'
            b = (b - 97 + shift) % 26 + 97
        end
        out[#out + 1] = string.char(b)
    end
    return table.concat(out)
end

for i = 2, #arg do
    local f = io.open(arg[i], "r")
    if not f then
        print("caesar: " .. arg[i] .. ": No such file or directory")
    else
        io.write(enc(f:read("*a") or ""))
        f:close()
    end
end

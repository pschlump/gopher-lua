-- grep.lua — print lines matching a Lua pattern, grep(1)-style.
-- Flags: -n line numbers, -v invert, -c count only, -i case-insensitive.
-- With more than one file, matches are prefixed "file:".

local opt_n, opt_v, opt_c, opt_i = false, false, false, false
local pattern = nil
local files = {}

local i = 1
while i <= #arg do
    local a = arg[i]
    if string.sub(a, 1, 1) == "-" and #a > 1 then
        for j = 2, #a do
            local c = string.sub(a, j, j)
            if c == "n" then opt_n = true
            elseif c == "v" then opt_v = true
            elseif c == "c" then opt_c = true
            elseif c == "i" then opt_i = true
            else
                print("grep: invalid option -- '" .. c .. "'")
                print("usage: grep [-nvci] pattern [file ...]")
                return
            end
        end
    elseif pattern == nil then
        pattern = a
    else
        files[#files + 1] = a
    end
    i = i + 1
end

if pattern == nil then
    print("usage: grep [-nvci] pattern [file ...]")
    return
end
if opt_i then
    pattern = string.lower(pattern)
end

local prefix = #files > 1

local function matches(line)
    if opt_i then
        line = string.lower(line)
    end
    local m = string.find(line, pattern) ~= nil
    if opt_v then
        m = not m
    end
    return m
end

for fi = 1, #files do
    local f = io.open(files[fi], "r")
    if not f then
        print("grep: " .. files[fi] .. ": No such file or directory")
    else
        local lineno = 0
        local count = 0
        for line in f:lines() do
            lineno = lineno + 1
            if matches(line) then
                count = count + 1
                if not opt_c then
                    local where = ""
                    if prefix then
                        where = files[fi] .. ":"
                    end
                    if opt_n then
                        where = where .. string.format("%d:", lineno)
                    end
                    print(where .. line)
                end
            end
        end
        f:close()
        if opt_c then
            if prefix then
                print(files[fi] .. ":" .. string.format("%d", count))
            else
                print(string.format("%d", count))
            end
        end
    end
end

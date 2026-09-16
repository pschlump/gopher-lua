-- uniq.lua — collapse adjacent repeated lines, like uniq(1) (sort first
-- for full dedup — see sortl.lua). Flags: -c prefix each line with its
-- repeat count, -d print only repeated lines.

local opt_c, opt_d = false, false
local files = {}

for i = 1, #arg do
    local a = arg[i]
    if string.sub(a, 1, 1) == "-" and #a > 1 then
        for j = 2, #a do
            local c = string.sub(a, j, j)
            if c == "c" then opt_c = true
            elseif c == "d" then opt_d = true
            else
                print("uniq: invalid option -- '" .. c .. "'")
                print("usage: uniq [-cd] [file ...]")
                return
            end
        end
    else
        files[#files + 1] = a
    end
end

local function emit(line, count)
    if opt_d and count < 2 then
        return
    end
    if opt_c then
        print(string.format("%7d %s", count, line))
    else
        print(line)
    end
end

local opened = 0
for i = 1, #files do
    local f = io.open(files[i], "r")
    if not f then
        print("uniq: " .. files[i] .. ": No such file or directory")
    else
        opened = opened + 1
        local prev, count = nil, 0
        for line in f:lines() do
            if line == prev then
                count = count + 1
            else
                if prev ~= nil then
                    emit(prev, count)
                end
                prev, count = line, 1
            end
        end
        if prev ~= nil then
            emit(prev, count)
        end
        f:close()
    end
end

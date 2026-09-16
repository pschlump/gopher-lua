-- cat.lua — concatenate files, like cat(1). -n numbers the output lines.
-- Cross-engine rules (see README.md): print only, fixed wording for
-- missing files (io.open error texts differ per engine), no os.exit.

local number = false
local files = {}

for i = 1, #arg do
    local a = arg[i]
    if a == "-" then
        files[#files + 1] = a -- lone dash: a literal file name, like cat
    elseif string.sub(a, 1, 1) == "-" and #a > 1 then
        for j = 2, #a do
            local c = string.sub(a, j, j)
            if c ~= "n" then
                print("cat: invalid option -- '" .. c .. "'")
                print("usage: cat [-n] [file ...]")
                return
            end
        end
        number = true
    else
        files[#files + 1] = a
    end
end

local lineno = 0
for i = 1, #files do
    local f = io.open(files[i], "r")
    if not f then
        print("cat: " .. files[i] .. ": No such file or directory")
    else
        for line in f:lines() do
            lineno = lineno + 1
            if number then
                print(string.format("%6d  %s", lineno, line))
            else
                print(line)
            end
        end
        f:close()
    end
end

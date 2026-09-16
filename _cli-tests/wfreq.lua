-- wfreq.lua — word frequency: the top-N words across files.
-- Usage: wfreq.lua [-N] file...
-- Words are runs of letters ("%a+"), lowercased; ranking is by count
-- (descending) then alphabetically. table.sort is avoided (ledger 10u).

local topn = 10
local files = {}

for i = 1, #arg do
    local a = arg[i]
    if string.sub(a, 1, 1) == "-" and #a > 1 then
        local n = tonumber(string.sub(a, 2))
        if n == nil then
            print("wfreq: invalid option -- '" .. string.sub(a, 2) .. "'")
            print("usage: wfreq.lua [-N] file...")
            return
        end
        topn = n
    else
        files[#files + 1] = a
    end
end

local counts = {}
local total = 0

for i = 1, #files do
    local f = io.open(files[i], "r")
    if not f then
        print("wfreq: " .. files[i] .. ": No such file or directory")
    else
        for line in f:lines() do
            for w in string.gmatch(string.lower(line), "%a+") do
                counts[w] = (counts[w] or 0) + 1
                total = total + 1
            end
        end
        f:close()
    end
end

-- rank: count descending, then word ascending
local words = {}
for w in pairs(counts) do
    words[#words + 1] = w
end
for i = 2, #words do -- insertion sort with the two-key compare
    local w = words[i]
    local j = i - 1
    while j >= 1 and (counts[words[j]] < counts[w] or
                      (counts[words[j]] == counts[w] and words[j] > w)) do
        words[j + 1] = words[j]
        j = j - 1
    end
    words[j + 1] = w
end

local shown = #words
if shown > topn then
    shown = topn
end
for i = 1, shown do
    print(string.format("%6d %s", counts[words[i]], words[i]))
end
print(string.format("%d words total, %d distinct", total, #words))

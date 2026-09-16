-- sortl.lua — sort lines from files, like sort(1), with a merge sort
-- written in plain Lua (kept for recursion/closure coverage even though
-- table.sort works on wasm now — see tsort.lua for the table.sort gate).
-- Flags: -n numeric compare, -r reverse, -u unique (collapse equal lines).

local opt_n, opt_r, opt_u = false, false, false
local files = {}

for i = 1, #arg do
    local a = arg[i]
    if string.sub(a, 1, 1) == "-" and #a > 1 then
        for j = 2, #a do
            local c = string.sub(a, j, j)
            if c == "n" then opt_n = true
            elseif c == "r" then opt_r = true
            elseif c == "u" then opt_u = true
            else
                print("sort: invalid option -- '" .. c .. "'")
                print("usage: sort [-nru] [file ...]")
                return
            end
        end
    else
        files[#files + 1] = a
    end
end

local lines = {}
for i = 1, #files do
    local f = io.open(files[i], "r")
    if not f then
        print("sort: " .. files[i] .. ": No such file or directory")
    else
        for line in f:lines() do
            lines[#lines + 1] = line
        end
        f:close()
    end
end

local function key(s)
    if opt_n then
        return tonumber(s) or 0
    end
    return s
end

local function less(a, b)
    return key(a) < key(b)
end

-- stable merge sort: on ties the earlier line wins
local function merge(a, b)
    local out = {}
    local i, j = 1, 1
    while i <= #a and j <= #b do
        if less(b[j], a[i]) then
            out[#out + 1] = b[j]
            j = j + 1
        else
            out[#out + 1] = a[i]
            i = i + 1
        end
    end
    while i <= #a do
        out[#out + 1] = a[i]
        i = i + 1
    end
    while j <= #b do
        out[#out + 1] = b[j]
        j = j + 1
    end
    return out
end

local function msort(t)
    if #t <= 1 then
        return t
    end
    local mid = math.floor(#t / 2)
    local left, right = {}, {}
    for k = 1, mid do
        left[k] = t[k]
    end
    for k = mid + 1, #t do
        right[k - mid] = t[k]
    end
    return merge(msort(left), msort(right))
end

lines = msort(lines)

local out = {}
local prev = nil
for i = 1, #lines do
    local l = lines[i]
    if not (opt_u and prev == l) then
        out[#out + 1] = l
    end
    prev = l
end

if opt_r then
    local rev = {}
    for i = 1, #out do
        rev[i] = out[#out - i + 1]
    end
    out = rev
end

for i = 1, #out do
    print(out[i])
end

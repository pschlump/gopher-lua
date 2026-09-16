-- q8.lua — the N-queens puzzle. Usage: q8.lua [n]
-- Prints the first solution found as a board and the solution count.

local n = tonumber(arg[1]) or 8
if n < 1 then
    n = 1
end

local cols = {}     -- cols[r] = column of the queen in row r
local count = 0
local first = nil

local function safe(row, col)
    for r = 1, row - 1 do
        local c = cols[r]
        if c == col or c - r == col - row or c + r == col + row then
            return false
        end
    end
    return true
end

local function place(row)
    if row > n then
        count = count + 1
        if first == nil then
            first = {}
            for r = 1, n do
                first[r] = cols[r]
            end
        end
        return
    end
    for col = 1, n do
        if safe(row, col) then
            cols[row] = col
            place(row + 1)
        end
    end
end

place(1)

for r = 1, n do
    local row = {}
    for c = 1, n do
        if first[r] == c then
            row[#row + 1] = "Q"
        else
            row[#row + 1] = "."
        end
    end
    print(table.concat(row))
end
print(string.format("%d solutions for n=%d", count, n))

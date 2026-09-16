-- life.lua — Conway's Game of Life on a wrapping (torus) grid.
-- Usage: life.lua pattern.life [generations [print_every]]
-- Pattern files: '#' comment lines, then rows of 'O' (live) and '.'
-- (dead), all the same width. Rules: B3/S23.

local path = arg[1]
local gens = tonumber(arg[2]) or 1
local every = tonumber(arg[3]) or gens

if path == nil then
    print("usage: life.lua pattern.life [generations [print_every]]")
    return
end

local grid = {}
local f = io.open(path, "r")
if not f then
    print("life: " .. path .. ": No such file or directory")
    return
end
for line in f:lines() do
    local clean = string.gsub(line, "%s", "")
    if #clean > 0 and string.sub(clean, 1, 1) ~= "#" then
        grid[#grid + 1] = clean
    end
end
f:close()

if #grid == 0 then
    print("life: " .. path .. ": empty pattern")
    return
end

local h = #grid
local w = #grid[1]

local function live(r, c)
    return string.sub(grid[r], c, c) == "O"
end

local function population()
    local n = 0
    for r = 1, h do
        for c = 1, w do
            if live(r, c) then
                n = n + 1
            end
        end
    end
    return n
end

local function step()
    local next = {}
    for r = 1, h do
        local row = {}
        for c = 1, w do
            local n = 0
            for dr = -1, 1 do
                for dc = -1, 1 do
                    if not (dr == 0 and dc == 0) then
                        local rr = ((r - 1 + dr) % h) + 1
                        local cc = ((c - 1 + dc) % w) + 1
                        if live(rr, cc) then
                            n = n + 1
                        end
                    end
                end
            end
            if live(r, c) then
                row[#row + 1] = (n == 2 or n == 3) and "O" or "."
            else
                row[#row + 1] = (n == 3) and "O" or "."
            end
        end
        next[r] = table.concat(row)
    end
    grid = next
end

local function show(gen)
    print(string.format("-- generation %d (population %d) --", gen, population()))
    for r = 1, h do
        print(grid[r])
    end
end

show(0)
for g = 1, gens do
    step()
    if g % every == 0 or g == gens then
        show(g)
    end
end

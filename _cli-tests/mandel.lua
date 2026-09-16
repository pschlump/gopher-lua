-- mandel.lua — ASCII rendering of the Mandelbrot set.
-- Usage: mandel.lua [width [height [max_iter]]]
-- No floats are printed: iteration counts index a character ramp, so
-- the output is byte-identical across engines.

local width = tonumber(arg[1]) or 60
local height = tonumber(arg[2]) or 20
local max_iter = tonumber(arg[3]) or 80

if width < 1 then width = 1 end
if height < 1 then height = 1 end
if max_iter < 1 then max_iter = 1 end

local ramp = " .:-=+*#%@"
local last = #ramp

local total = 0
for py = 0, height - 1 do
    local row = {}
    local y0 = (py / height) * 3.0 - 1.5
    for px = 0, width - 1 do
        local x0 = (px / width) * 3.0 - 2.0
        local x, y = 0.0, 0.0
        local iter = 0
        while iter < max_iter and x * x + y * y <= 4.0 do
            local xt = x * x - y * y + x0
            y = 2.0 * x * y + y0
            x = xt
            iter = iter + 1
        end
        total = total + iter
        local idx = math.floor(iter / max_iter * last) + 1
        if idx < 1 then
            idx = 1
        elseif idx > last then
            idx = last
        end
        row[#row + 1] = string.sub(ramp, idx, idx)
    end
    io.write(table.concat(row), "\n")
end
io.write(string.format("total iterations: %d\n", total))

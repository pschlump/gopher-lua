-- bf.lua — a brainfuck interpreter.
-- Usage: bf.lua program.bf [program.bf ...]
-- Cells are bytes (mod 256), the tape grows to the right only, and ','
-- (input) is not supported — programs must be self-contained. Prints
-- each program's output and a step count.

local function parse(program)
    local code = {}
    for i = 1, #program do
        local c = string.sub(program, i, i)
        if c == "+" or c == "-" or c == "<" or c == ">" or
           c == "." or c == "[" or c == "]" then
            code[#code + 1] = c
        end
    end
    -- precompute the bracket jump map
    local jump = {}
    local stack = {}
    for i = 1, #code do
        local c = code[i]
        if c == "[" then
            stack[#stack + 1] = i
        elseif c == "]" then
            if #stack == 0 then
                return nil, "unbalanced ]"
            end
            local j = stack[#stack]
            stack[#stack] = nil
            jump[i] = j
            jump[j] = i
        end
    end
    if #stack > 0 then
        return nil, "unbalanced ["
    end
    return code, jump
end

local function run(program)
    local code, jump = parse(program)
    if code == nil then
        return nil, 0, jump
    end
    local tape = {}
    local head = 1
    local out = {}
    local pc = 1
    local steps = 0
    local limit = 10000000
    while pc <= #code do
        steps = steps + 1
        if steps > limit then
            return nil, steps, "step limit exceeded"
        end
        local c = code[pc]
        local v = tape[head] or 0
        if c == "+" then
            tape[head] = (v + 1) % 256
        elseif c == "-" then
            tape[head] = (v - 1) % 256
        elseif c == ">" then
            head = head + 1
        elseif c == "<" then
            head = head - 1
            if head < 1 then
                return nil, steps, "tape underflow"
            end
        elseif c == "." then
            out[#out + 1] = string.char(v)
        elseif c == "[" then
            if v == 0 then
                pc = jump[pc]
            end
        elseif c == "]" then
            if v ~= 0 then
                pc = jump[pc]
            end
        end
        pc = pc + 1
    end
    return table.concat(out), steps, nil
end

for i = 1, #arg do
    local f = io.open(arg[i], "r")
    if not f then
        print("bf: " .. arg[i] .. ": No such file or directory")
    else
        local src = f:read("*a") or ""
        f:close()
        local text, steps, err = run(src)
        if text == nil then
            io.write("bf: " .. arg[i] .. ": " .. err .. "\n")
        else
            -- one output channel throughout: print events and WASI
            -- stdout keep separate orders in the wasm engines
            io.write(text)
            if string.sub(text, #text) ~= "\n" then
                io.write("\n")
            end
            io.write(string.format("[%s: %d steps]\n", arg[i], steps))
        end
    end
end

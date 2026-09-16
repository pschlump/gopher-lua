-- #!/usr/bin/env lua

-- Execution flags
local show_lines = false
local show_words = false
local show_bytes = false

-- Runtime tracking
local files = {}
local total_lines = 0
local total_words = 0
local total_bytes = 0

-- 1. Parse Command Line Arguments
for i = 1, #arg do
    local argument = arg[i]
    if string.sub(argument, 1, 1) == "-" and #argument > 1 then
        -- Process flag clusters (e.g., -lwc)
        for j = 2, #argument do
            local char = string.sub(argument, j, j)
            if char == "l" then show_lines = true
            elseif char == "w" then show_words = true
            elseif char == "c" then show_bytes = true
            else
                io.stderr:write(string.format("lwc: invalid option -- '%s'\n", char))
                io.stderr:write("Usage: lwc [-lwc] [file ...]\n")
                os.exit(1)
            end
        end
    else
        -- Collect file paths
        table.insert(files, argument)
    end
end

-- Fallback to POSIX default configuration if no switches were supplied
if not show_lines and not show_words and not show_bytes then
    show_lines, show_words, show_bytes = true, true, true
end

-- 2. Clean Formatted Output Renderer
local function print_counts(lines, words, bytes, name)
    local out = {}
    if show_lines then table.insert(out, string.format("%7d", lines)) end
    if show_words then table.insert(out, string.format("%7d", words)) end
    if show_bytes then table.insert(out, string.format("%7d", bytes)) end
    
    local line_str = table.concat(out, " ")
    if name then
        print(string.format("%s %s", line_str, name))
    else
        print(line_str)
    end
end

-- 3. Chunked File Stream Processor
local function process_file(fh, name)
    local lines, words, bytes = 0, 0, 0
    local in_word = false
    local chunk_size = 65536 -- 64KB Buffer

    while true do
        local chunk = fh:read(chunk_size)
        if not chunk or #chunk == 0 then break end

        bytes = bytes + #chunk

        for i = 1, #chunk do
            local b = string.byte(chunk, i)

            if b == 10 then -- '\n'
                lines = lines + 1
            end

            -- Match POSIX space definitions: ' ', '\t', '\n', '\v', '\f', '\r'
            if b == 32 or (b >= 9 and b <= 13) then
                in_word = false
            else
                if not in_word then
                    words = words + 1
                    in_word = true
                end
            end
        end
    end

    print_counts(lines, words, bytes, name)

    -- Track metrics for global summary
    total_lines = total_lines + lines
    total_words = total_words + words
    total_bytes = total_bytes + bytes
end

-- 4. Main Execution Engine
if #files == 0 then
    process_file(io.stdin)
else
    for i = 1, #files do
        local filename = files[i]
        local file, err = io.open(filename, "r")
        
        if not file then
            io.stderr:write(string.format("lwc: %s: %s\n", filename, err or "No such file"))
        else
            process_file(file, filename)
            file:close()
        end
    end

    if #files > 1 then
        print_counts(total_lines, total_words, total_bytes, "total")
    end
end


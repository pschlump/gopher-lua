-- #!/usr/bin/env lua

-- Global totals
local total_lines = 0
local total_words = 0
local total_bytes = 0
local file_count  = #arg

-- Renders output in the standard right-aligned wc layout
local function print_counts(lines, words, bytes, name)
    print(string.format("%7d %7d %7d %s", lines, words, bytes, name or ""))
end

-- Process an individual file stream
local function process_file(fh, name)
    local lines, words, bytes = 0, 0, 0
    local in_word = false

    -- 64KB chunk buffer size for memory efficiency and high performance
    local chunk_size = 65536 

    while true do
        local chunk = fh:read(chunk_size)
        if not chunk or #chunk == 0 then break end

        bytes = bytes + #chunk

        -- Interate over the chunk byte by byte to handle counters cleanly
        for i = 1, #chunk do
            local b = string.byte(chunk, i)

            -- Check for newline (\n is ASCII 10)
            if b == 10 then
                lines = lines + 1
            end

            -- Match standard POSIX whitespace: space(32), \t(9), \n(10), \v(11), \f(12), \r(13)
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

    -- Accumulate globals
    total_lines = total_lines + lines
    total_words = total_words + words
    total_bytes = total_bytes + bytes
end

-- Main Execution Block
if file_count == 0 then
    -- Fallback to standard input if no file arguments are supplied
    process_file(io.stdin)
else
    -- Iterate through files passed via CLI args
    for i = 1, file_count do
        local filename = arg[i]
        local file, err = io.open(filename, "r")
        
        if not file then
            io.stderr:write(string.format("lwc: %s: %s\n", filename, err or "No such file or directory"))
        else
            process_file(file, filename)
            file:close()
        end
    end

    -- Only show the grand total row if evaluating multiple targets
    if file_count > 1 then
        print_counts(total_lines, total_words, total_bytes, "total")
    end
end


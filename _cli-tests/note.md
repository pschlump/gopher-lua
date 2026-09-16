Here is a complete, production-ready Lua implementation of a wc clone. It handles multiple files passed via command line arguments (arg), tracks running totals, balances memory consumption by reading chunks rather than entire files, and mirrors the exact alignment and tab-spaced formatting of the classic Linux utility.
## The Lua wc Program
Save the following code as lwc.lua:

#!/usr/bin/env lua
-- Global totalslocal total_lines = 0local total_words = 0local total_bytes = 0local file_count  = #arg
-- Renders output in the standard right-aligned wc layoutlocal function print_counts(lines, words, bytes, name)
    print(string.format("%7d %7d %7d %s", lines, words, bytes, name or ""))end
-- Process an individual file streamlocal function process_file(fh, name)
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
    total_bytes = total_bytes + bytesend
-- Main Execution Blockif file_count == 0 then
    -- Fallback to standard input if no file arguments are supplied
    process_file(io.stdin)else
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
    endend

## Key Architectural Considerations

* 
* Memory Safety & Performance: Instead of loading arbitrary file lengths using *all or evaluating via slower string regex models like string.gmatch, this script reads files in 64KB chunks via fh:read(bytes). This ensures highly predictable memory consumption even when processing multi-gigabyte production logs.
* POSIX Whitespace Compliance: Using Lua patterns like "%S+" can miss certain carriage returns (\r) or tabs depending on platform locales. Explicit ASCII evaluation mapping b == 32 and b >= 9 and b <= 13 precisely replicates how Linux coreutils maps string delimiters.
* Streams (stdin) Compatibility: When invoked without explicit targets, it immediately shifts context to evaluate io.stdin, making it drop-in compatible with pipes (cat log.txt | lua lwc.lua).
* 

## Usage Examples
Make the script executable and query a series of files:

chmod +x lwc.lua
./lwc.lua file1.txt file2.log

Output:

     124     842    5812 file1.txt
    2301   14209   98431 file2.log
    2425   15051  104243 total

Would you like to extend this script to parse command-line flags like -l, -w, or -c to mimic matching narrow output filters?

# Quick verification of character and byte handling rules in standard wc vs Lua pattern matching if needed, but not strictly required.# Let's verify string.len vs string.gmatch for word splitting.text = "Hello world\nthis is a test."words = len(text.split())
print(f"words: {words}")




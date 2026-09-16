-- kvdb.lua — a tiny persistent key-value store, Redis-flavored.
-- Usage: kvdb.lua dbfile command [args...]
--   set key value     store key=value
--   get key           print the value (or <nil>)
--   del key           remove a key
--   incr key [delta]  add delta (default 1) to the numeric value
--   keys [prefix]     print matching keys in sorted order
--
-- The db is a plain text file of "key=value" lines. Keys and values
-- must be printable and may not contain '=' (except one), newline or
-- NUL. Errors are raised with error(msg, 0) — no position prefix — so
-- messages are byte-identical across engines (clua-run shifts lines).

local function fail(msg)
    error(msg, 0)
end

-- insertion sort over strings: kept for plain-Lua coverage (table.sort
-- works on wasm — ledger row 10u was fixed with the M6 stack fixes)
local function sort_keys(keys)
    for i = 2, #keys do
        local k = keys[i]
        local j = i - 1
        while j >= 1 and keys[j] > k do
            keys[j + 1] = keys[j]
            j = j - 1
        end
        keys[j + 1] = k
    end
end

local function load(path)
    local t = {}
    local f = io.open(path, "r")
    if not f then
        return t -- a missing db is an empty db
    end
    for line in f:lines() do
        local eq = string.find(line, "=", 1, true)
        if eq then
            t[string.sub(line, 1, eq - 1)] = string.sub(line, eq + 1)
        end
    end
    f:close()
    return t
end

local function save(path, t)
    local keys = {}
    for k in pairs(t) do
        keys[#keys + 1] = k
    end
    sort_keys(keys)
    local f = io.open(path, "w")
    if not f then
        fail("kvdb: cannot write " .. path)
    end
    for i = 1, #keys do
        f:write(keys[i], "=", t[keys[i]], "\n")
    end
    f:close()
end

local function check(s, what)
    if string.find(s, "=", 1, true) or string.find(s, "\n") or string.find(s, "\0") then
        fail("kvdb: " .. what .. " may not contain '=', newline or NUL")
    end
end

local function main()
    local dbpath, cmd = arg[1], arg[2]
    if dbpath == nil or cmd == nil then
        fail("usage: kvdb.lua dbfile command [args...]")
    end
    local t = load(dbpath)

    if cmd == "set" then
        local k, v = arg[3], arg[4]
        if k == nil or v == nil or arg[5] ~= nil then
            fail("kvdb: set takes exactly two arguments")
        end
        check(k, "key")
        check(v, "value")
        t[k] = v
        save(dbpath, t)
        print("ok")
    elseif cmd == "get" then
        local k = arg[3]
        if k == nil or arg[4] ~= nil then
            fail("kvdb: get takes exactly one argument")
        end
        if t[k] == nil then
            print("<nil>")
        else
            print(t[k])
        end
    elseif cmd == "del" then
        local k = arg[3]
        if k == nil or arg[4] ~= nil then
            fail("kvdb: del takes exactly one argument")
        end
        if t[k] == nil then
            print("(0)")
        else
            t[k] = nil
            save(dbpath, t)
            print("(1)")
        end
    elseif cmd == "incr" then
        local k = arg[3]
        local d = tonumber(arg[4] or "1")
        if k == nil or arg[5] ~= nil or d == nil then
            fail("kvdb: incr takes a key and a numeric delta")
        end
        local cur = tonumber(t[k]) or 0
        if t[k] ~= nil and tostring(cur) ~= t[k] then
            fail("kvdb: value is not an integer")
        end
        t[k] = string.format("%d", cur + d)
        save(dbpath, t)
        print(string.format("%d", cur + d))
    elseif cmd == "keys" then
        if arg[4] ~= nil then
            fail("kvdb: keys takes at most one prefix")
        end
        local prefix = arg[3] or ""
        local keys = {}
        for k in pairs(t) do
            if string.sub(k, 1, #prefix) == prefix then
                keys[#keys + 1] = k
            end
        end
        sort_keys(keys)
        for i = 1, #keys do
            print(keys[i])
        end
    else
        fail("kvdb: unknown command: " .. cmd)
    end
end

local ok, err = pcall(main)
if not ok then
    print(tostring(err))
end

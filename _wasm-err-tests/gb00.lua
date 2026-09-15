print(pcall(function() return ('x'):gsub('x', function() error('gsub boom') end) end))

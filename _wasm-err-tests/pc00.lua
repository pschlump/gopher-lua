print(pcall(function() print(pcall(function() error('inner') end)) error('outer') end))

local ok, e = pcall(function() error('x') end)
print(ok, e)

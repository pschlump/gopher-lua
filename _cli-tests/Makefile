
.PHONY: setup test000 test001 fin

test: setup test000 test001 test002 fin

setup:
	mkdir -p out ref tmp data

fin:
	@echo PASS

test000:
	@echo test000
	../cmd/glua/glua -w tmp/lwc2.wasm ./lwc2.lua 
	../cmd/glua/glua -W tmp/lwc2.wasm data/* >./out/out001.out
	diff ./out/out001.out ./ref/out001.out

test001:
	@echo test001
	../cmd/glua/glua ./lwc2.lua data/* >./out/out002.out
	diff ./out/out002.out ./ref/out002.out

test002:
	@echo test000
	../cmd/glua/glua -w tmp/lwc2.wasm ./lwc2.lua 
	../cmd/glua/glua -W tmp/lwc2.wasm -w data/* >./out/out003.out
	diff ./out/out003.out ./ref/out003.out


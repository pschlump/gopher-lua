module github.com/pschlump/gopher-lua

go 1.27.0

require (
	github.com/bytecodealliance/wasmtime-go/v48 v48.0.0
	github.com/ergochat/readline v0.1.3
	github.com/pschlump/wazero v1.12.0
)

// Local fork of wazero (upstream github.com/tetratelabs/wazero, module path renamed).
replace github.com/pschlump/wazero => ../wazero

require (
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.9.0 // indirect
)

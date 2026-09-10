package testdiff

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/pschlump/gopher-lua"
	"github.com/pschlump/gopher-lua/parse"
)

func TestDumpScript(t *testing.T) {
	src := "for i = 1, 3 do end\n"
	bin, err := CompileSource([]byte(src), "d.lua")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("/tmp/d.wasm", bin, 0644); err != nil {
		t.Fatal(err)
	}
	chunk, _ := parse.Parse(strings.NewReader(src), "d.lua")
	proto, _ := lua.Compile(chunk, "d.lua")
	for i, cv := range proto.Constants {
		t.Logf("K[%d]=%#v sc[%d]=%q", i, cv, i, proto.StringConstants()[i])
	}
	for pc, inst := range proto.Code {
		op := int(inst >> 26)
		info := ""
		switch op {
		case lua.OP_JMP, lua.OP_FORLOOP, lua.OP_FORPREP:
			sbx := int(inst&0x3ffff) - (1<<18-1)/2
			info = fmt.Sprintf(" -> target %d", pc+1+sbx)
		}
		t.Logf("pc=%d op=%d(%s) A=%d B=%d C=%d%s", pc, op, opname(op),
			(inst>>18)&0xff, inst&0x1ff, (inst>>9)&0x1ff, info)
	}
}

func opname(op int) string {
	names := map[int]string{lua.OP_MOVE: "MOVE", lua.OP_MOVEN: "MOVEN", lua.OP_LOADK: "LOADK", lua.OP_LOADBOOL: "LOADBOOL", lua.OP_LOADNIL: "LOADNIL", lua.OP_GETUPVAL: "GETUPVAL", lua.OP_GETGLOBAL: "GETGLOBAL", lua.OP_GETTABLE: "GETTABLE", lua.OP_GETTABLEKS: "GETTABLEKS", lua.OP_SETGLOBAL: "SETGLOBAL", lua.OP_SETUPVAL: "SETUPVAL", lua.OP_SETTABLE: "SETTABLE", lua.OP_SETTABLEKS: "SETTABLEKS", lua.OP_NEWTABLE: "NEWTABLE", lua.OP_SELF: "SELF", lua.OP_ADD: "ADD", lua.OP_SUB: "SUB", lua.OP_MUL: "MUL", lua.OP_DIV: "DIV", lua.OP_MOD: "MOD", lua.OP_POW: "POW", lua.OP_UNM: "UNM", lua.OP_NOT: "NOT", lua.OP_LEN: "LEN", lua.OP_CONCAT: "CONCAT", lua.OP_JMP: "JMP", lua.OP_EQ: "EQ", lua.OP_LT: "LT", lua.OP_LE: "LE", lua.OP_TEST: "TEST", lua.OP_TESTSET: "TESTSET", lua.OP_TFORLOOP: "TFORLOOP", lua.OP_SETLIST: "SETLIST", lua.OP_CLOSE: "CLOSE", lua.OP_CLOSURE: "CLOSURE", lua.OP_VARARG: "VARARG", lua.OP_NOP: "NOP", lua.OP_RETURN: "RETURN", lua.OP_TAILCALL: "TAILCALL"}
	if n, ok := names[op]; ok {
		return n
	}
	return "?"
}

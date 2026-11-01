package main

import (
	"flag"
	"fmt"
	"os"
	"runtime/pprof"

	"github.com/ergochat/readline"
	lua "github.com/pschlump/gopher-lua"
	"github.com/pschlump/gopher-lua/parse"
)

func main() {
	os.Exit(mainAux())
}

func mainAux() int {
	var opt_e, opt_l, opt_p string
	var opt_i, opt_v, opt_dt, opt_dc bool
	// Every option takes its single-letter and a full-word spelling (-v,
	// -version); both names bind the same variable, and Go's flag package
	// already accepts -name and --name alike.
	flag.StringVar(&opt_e, "e", "", "")
	flag.StringVar(&opt_e, "execute", "", "")
	flag.StringVar(&opt_l, "l", "", "")
	flag.StringVar(&opt_l, "require", "", "")
	flag.StringVar(&opt_p, "p", "", "")
	flag.StringVar(&opt_p, "cpuprofile", "", "")
	flag.BoolVar(&opt_i, "i", false, "")
	flag.BoolVar(&opt_i, "interactive", false, "")
	flag.BoolVar(&opt_v, "v", false, "")
	flag.BoolVar(&opt_v, "version", false, "")
	flag.BoolVar(&opt_dt, "dt", false, "")
	flag.BoolVar(&opt_dt, "dumpast", false, "")
	flag.BoolVar(&opt_dc, "dc", false, "")
	flag.BoolVar(&opt_dc, "dumpcode", false, "")
	flag.Usage = func() {
		fmt.Println(`Usage: glua [options] [script [args]].
Available options are:
  -e stat,  -execute stat      execute string 'stat'
  -l name,  -require name      require library 'name'
  -dt,      -dumpast           dump AST trees
  -dc,      -dumpcode          dump VM codes
  -i,       -interactive       enter interactive mode after executing 'script'
  -p file,  -cpuprofile file   write cpu profiles to the file
  -v,       -version           show version information (git tag/commit, build date)`)
	}
	flag.Parse()
	if len(opt_p) != 0 {
		f, err := os.Create(opt_p)
		if err != nil {
			fmt.Println(err.Error())
			os.Exit(1)
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}
	if len(opt_e) == 0 && !opt_i && !opt_v && flag.NArg() == 0 {
		opt_i = true
	}

	status := 0

	L := lua.NewState()
	defer L.Close()
	// -v: version banner (release line + link-time git tag/commit/date);
	// -i prints the same banner before entering the REPL.
	if opt_v || opt_i {
		fmt.Println(versionInfo())
	}

	if len(opt_l) > 0 {
		if err := L.DoFile(opt_l); err != nil {
			fmt.Println(err.Error())
		}
	}

	if nargs := flag.NArg(); nargs > 0 {
		script := flag.Arg(0)
		argtb := L.NewTable()
		// the standalone surface: arg[0] = script name, arg[1..n] = CLI
		// args (U5f note: this is the standalone interpreter's argv table,
		// which stock 5.5 keeps; it is NOT the 5.1 vararg 'arg' that
		// U5b removed from function bodies)
		L.RawSet(argtb, lua.LNumber(0), lua.LString(script))
		for i := 1; i < nargs; i++ {
			L.RawSet(argtb, lua.LNumber(i), lua.LString(flag.Arg(i)))
		}
		L.SetGlobal("arg", argtb)
		if opt_dt || opt_dc {
			file, err := os.Open(script)
			if err != nil {
				fmt.Println(err.Error())
				return 1
			}
			chunk, err2 := parse.Parse(file, script)
			if err2 != nil {
				fmt.Println(err2.Error())
				return 1
			}
			if opt_dt {
				fmt.Println(parse.Dump(chunk))
			}
			if opt_dc {
				proto, err3 := lua.Compile(chunk, script)
				if err3 != nil {
					fmt.Println(err3.Error())
					return 1
				}
				fmt.Println(proto.String())
			}
		}
		if err := L.DoFile(script); err != nil {
			fmt.Println(err.Error())
			status = 1
		}
	}

	if len(opt_e) > 0 {
		if err := L.DoString(opt_e); err != nil {
			fmt.Println(err.Error())
			status = 1
		}
	}

	if opt_i {
		doREPL(L)
	}
	return status
}

// do read/eval/print/loop
func doREPL(L *lua.LState) {
	rl, err := readline.New("> ")
	if err != nil {
		panic(err)
	}
	defer rl.Close()
	for {
		if str, err := loadline(rl, L); err == nil {
			if err := L.DoString(str); err != nil {
				fmt.Println(err)
			}
		} else { // error on loadline
			fmt.Println(err)
			return
		}
	}
}

func incomplete(err error) bool {
	if lerr, ok := err.(*lua.ApiError); ok {
		if perr, ok := lerr.Cause.(*parse.Error); ok {
			return perr.Pos.Line == parse.EOF
		}
	}
	return false
}

func loadline(rl *readline.Instance, L *lua.LState) (string, error) {
	rl.SetPrompt("> ")
	if line, err := rl.Readline(); err == nil {
		if _, err := L.LoadString("return " + line); err == nil { // try add return <...> then compile
			return line, nil
		} else {
			return multiline(line, rl, L)
		}
	} else {
		return "", err
	}
}

func multiline(ml string, rl *readline.Instance, L *lua.LState) (string, error) {
	for {
		if _, err := L.LoadString(ml); err == nil { // try compile
			return ml, nil
		} else if !incomplete(err) { // syntax error , but not EOF
			return ml, nil
		} else {
			rl.SetPrompt(">> ")
			if line, err := rl.Readline(); err == nil {
				ml = ml + "\n" + line
			} else {
				return "", err
			}
		}
	}
}

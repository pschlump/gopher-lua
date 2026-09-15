// Command testdiff runs a corpus across engines and reports log diffs.
// CI entry point for the differential harness (design doc §7, §8):
//
//	go run ./cmd/testdiff -corpus _glua-tests -engines interp,interp
//
// Exit code 1 on any diff. Engine names are currently interp (the
// gopher-lua oracle); C-Lua-wasm (M2) and the wasm backend (M4) plug in
// here without changing the invocation.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/pschlump/gopher-lua/testdiff"
)

func main() {
	corpus := flag.String("corpus", "_glua-tests", "corpus directory")
	enginesFlag := flag.String("engines", "interp,interp", "comma-separated engine list")
	verbose := flag.Bool("v", false, "print full event logs")
	flag.Parse()

	var engines []testdiff.Engine
	for i, name := range strings.Split(*enginesFlag, ",") {
		switch name {
		case "clua":
			engines = append(engines, testdiff.NewCLua(fmt.Sprintf("clua-%c", 'a'+i)))
		case "interp":
			engines = append(engines, testdiff.NewInterp(fmt.Sprintf("interp-%c", 'a'+i)))
		case "wasm":
			e := testdiff.NewWasmEngine(fmt.Sprintf("wasm-%c", 'a'+i))
			e.SkipUnsupported = true
			engines = append(engines, e)
		default:
			fmt.Fprintf(os.Stderr, "unknown engine %q (known: interp, clua, wasm)\n", name)
			os.Exit(2)
		}
	}

	cases, err := testdiff.LoadCorpus(*corpus)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load corpus: %v\n", err)
		os.Exit(2)
	}
	cases, skippedList := testdiff.FilterSkips(cases, map[string]string{
		// Ledgered divergences (docs/Lua-Wasm-Divergence-Ledger.md):
		// error-message wording differs between the gopher-lua oracle and
		// stock C Lua 5.1 (the M2 oracle contract keeps the C texts);
		// table.sort's comparator path still trips the wasm callback
		// machinery (ledger row 10 — carried to M5d; gsub/pcall/__index
		// callbacks pass since the A4 adapter).
		"err00.lua": "error-message wording (gopher-lua vs C 5.1) — M5 byte-exact suite",
		"err01.lua": "error-message wording (gopher-lua vs C 5.1) — M5 byte-exact suite",
		"err02.lua": "error-message wording (gopher-lua vs C 5.1) — M5 byte-exact suite",
		"tbl07.lua": "table.sort callback path — ledger row 10, M5d",
		"cb00.lua":  "table.sort comparator callback — ledger row 10, M5d",
	})
	results := testdiff.RunCorpus(cases, engines)

	diffs := 0
	for i := range results {
		r := &results[i]
		if skipUnsupported(r) {
			r.Skipped = true
			skippedList = append(skippedList, r.Name+": "+r.SkipWhy)
			if *verbose {
				fmt.Printf("SKIP-UNSUPPORTED %s: %s\n", r.Name, r.SkipWhy)
			}
			continue
		}
		if d := r.Diff(); d != "" {
			diffs++
			fmt.Printf("DIFF    %s\n%s", r.Name, d)
		} else if *verbose {
			fmt.Printf("== %s ==\n", r.Name)
			for _, line := range r.Logs[r.Engines[0]] {
				fmt.Println(line)
			}
		}
	}
	fmt.Print(testdiff.Summary(results, skippedList))
	if diffs > 0 {
		os.Exit(1)
	}
}

// skipUnsupported marks a result skipped when any engine log is a
// SKIP-UNSUPPORTED marker (v1 backend: closures/varargs arrive with M5).
func skipUnsupported(r *testdiff.Result) bool {
	for _, eng := range r.Engines {
		if log := r.Logs[eng]; len(log) > 0 && strings.HasPrefix(log[0], "SKIP-UNSUPPORTED") {
			r.SkipWhy = strings.TrimPrefix(log[0], "SKIP-UNSUPPORTED\t")
			return true
		}
	}
	return false
}

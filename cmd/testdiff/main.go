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
		case "interp":
			engines = append(engines, testdiff.NewInterp(fmt.Sprintf("interp-%c", 'a'+i)))
		default:
			fmt.Fprintf(os.Stderr, "unknown engine %q (known: interp)\n", name)
			os.Exit(2)
		}
	}

	cases, err := testdiff.LoadCorpus(*corpus)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load corpus: %v\n", err)
		os.Exit(2)
	}
	cases, skipped := testdiff.FilterSkips(cases, map[string]string{})
	results := testdiff.RunCorpus(cases, engines)

	diffs := 0
	for _, r := range results {
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
	fmt.Print(testdiff.Summary(results, skipped))
	if diffs > 0 {
		os.Exit(1)
	}
}

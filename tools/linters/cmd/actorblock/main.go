// Command actorblock runs the actorblock analyzer.
//
// Invoked by "go vet -vettool" it behaves like any other vet tool. Run
// directly with package patterns it analyzes the whole program in one
// process, which lets it do what a per-package vet run cannot: check that
// every baseline entry is still needed.
//
//	actorblock -baseline actorblock_baseline.txt ./... ./baselib/...
//	actorblock -write-baseline ./... > actorblock_baseline.txt
//
// The exit status is non-zero when there are findings or stale baseline
// entries.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lightninglabs/wavelength/tools/linters/actorblock"
	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/checker"
	"golang.org/x/tools/go/analysis/unitchecker"
	"golang.org/x/tools/go/packages"
)

func main() {
	// go vet runs the tool with -V=full, -flags, or a single .cfg file.
	if isVetInvocation(os.Args[1:]) {
		unitchecker.Main(actorblock.Analyzer)
	}

	os.Exit(run())
}

// isVetInvocation reports whether args look like a go vet tool call.
func isVetInvocation(args []string) bool {
	if len(args) == 0 {
		return false
	}

	last := args[len(args)-1]
	if strings.HasSuffix(last, ".cfg") {
		return true
	}

	return args[0] == "-V=full" || args[0] == "-flags"
}

// run is the standalone driver. It returns the process exit code.
func run() int {
	var (
		baselinePath = flag.String(
			"baseline", "", "baseline file of tolerated functions",
		)
		actorPkg = flag.String(
			"actor-pkg", actorblock.DefaultActorPkg,
			"import path of the actor package",
		)
		entryMethods = flag.String(
			"entry-methods", "",
			"comma-separated Method@pkgpath.Interface extra entry "+
				"points (default "+
				strings.Join(
					actorblock.DefaultEntryMethods, ",",
				)+")",
		)
		tags = flag.String(
			"tags", "", "comma-separated build tags",
		)
		write = flag.Bool(
			"write-baseline", false,
			"print a baseline for the current tree and exit",
		)
		noStale = flag.Bool(
			"no-stale-check", false, "do not fail on stale "+
				"baseline entries, for partial package sets",
		)
	)
	flag.Parse()

	patterns := flag.Args()
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}

	var baseline *actorblock.Baseline
	if *baselinePath != "" && !*write {
		var err error
		baseline, err = actorblock.LoadBaseline(*baselinePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "actorblock:", err)

			return 2
		}
	}

	rec := actorblock.NewRecorder()
	cfg := &actorblock.Config{
		ActorPkg: *actorPkg,
		Baseline: baseline,
		Recorder: rec,
	}
	if *entryMethods != "" {
		cfg.EntryMethods = strings.Split(*entryMethods, ",")
	}
	if baseline == nil {
		// An empty baseline stops the config from loading a path.
		cfg.Baseline = actorblock.NewBaseline()
	}
	analyzer := actorblock.NewAnalyzer(cfg)

	pkgCfg := &packages.Config{
		Mode: packages.LoadAllSyntax | packages.NeedModule,
	}
	if *tags != "" {
		pkgCfg.BuildFlags = []string{"-tags=" + *tags}
	}

	pkgs, err := packages.Load(pkgCfg, patterns...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "actorblock:", err)

		return 2
	}

	// Type errors make the result unreliable, so refuse to guess.
	var loadErrs int
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		for _, e := range p.Errors {
			fmt.Fprintln(os.Stderr, e)
			loadErrs++
		}
	})
	if loadErrs > 0 {
		return 2
	}

	graph, err := checker.Analyze([]*analysis.Analyzer{analyzer}, pkgs, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "actorblock:", err)

		return 2
	}

	if *write {
		fmt.Print(actorblock.FormatBaseline(rec.Needed()))

		return 0
	}

	var findings []string
	for _, act := range graph.Roots {
		if act.Err != nil {
			fmt.Fprintf(
				os.Stderr, "actorblock: %v: %v\n", act, act.Err,
			)

			return 2
		}

		for _, d := range act.Diagnostics {
			pos := act.Package.Fset.Position(d.Pos)
			findings = append(
				findings,
				fmt.Sprintf(
					"%s:%d:%d: %s", relPath(pos.Filename),
					pos.Line, pos.Column, d.Message,
				),
			)
		}
	}
	sort.Strings(findings)
	for _, f := range findings {
		fmt.Println(f)
	}

	var stale []string
	if baseline != nil && !*noStale {
		stale = baseline.Stale(rec.Needed())
		for _, key := range stale {
			fmt.Printf("%s: stale actorblock baseline entry, "+
				"remove it\n",
				key)
		}
	}

	if len(findings) > 0 || len(stale) > 0 {
		return 1
	}

	return 0
}

// relPath shortens an absolute path against the working directory.
func relPath(path string) string {
	wd, err := os.Getwd()
	if err != nil {
		return path
	}

	if rel, err := filepath.Rel(wd, path); err == nil {
		return rel
	}

	return path
}

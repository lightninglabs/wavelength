package actorblock

import (
	"flag"
	"fmt"
	"go/ast"
	"go/types"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/types/typeutil"
)

const (
	// DefaultActorPkg is the import path of the actor framework.
	DefaultActorPkg = "github.com/lightninglabs/wavelength/baselib/actor"

	// DefaultProtofsmPkg is the import path of the state machine package.
	DefaultProtofsmPkg = "github.com/lightninglabs/wavelength/baselib/" +
		"protofsm"

	// KindAwait marks a Future.Await call site.
	KindAwait = "await"

	// KindSend marks a Tell or Ask made with a non-turn context.
	KindSend = "send"

	allowAwaitDirective = "//actor:allow-await"
	allowSendDirective  = "//actor:allow-send"
)

// Site is a blocking call site that a function can reach.
type Site struct {
	// Kind is KindAwait or KindSend.
	Kind string

	// Key is the full name of the function that directly contains the
	// call, which is also the baseline key.
	Key string

	// Count is the number of direct sites of this kind in the function
	// Key, which is what a baseline entry bounds. Callers carry it
	// unchanged.
	Count int

	// Chain describes the path from the function holding the fact to the
	// call, for example "pkg.helper -> Future.Await at file.go:12".
	Chain string
}

// MayBlock is a fact attached to a function that can reach blocking sites
// through statically resolved calls. Sites holds one entry per distinct
// (Kind, Key), carrying the shortest known chain.
type MayBlock struct {
	Sites []Site
}

// AFact marks MayBlock as an analysis fact.
func (*MayBlock) AFact() {}

// String renders the fact for the analysis driver's debug output.
func (f *MayBlock) String() string {
	keys := make([]string, 0, len(f.Sites))
	for _, s := range f.Sites {
		keys = append(keys, s.Kind+":"+s.Key)
	}

	return "mayBlock(" + strings.Join(keys, ",") + ")"
}

// Config holds the settings of one analyzer instance.
type Config struct {
	// ActorPkg is the import path of the actor framework. It defaults to
	// DefaultActorPkg.
	ActorPkg string

	// BaselinePath is the baseline file. It is loaded on first use. An
	// empty path means no baseline.
	BaselinePath string

	// EntryMethods lists extra entry point methods as
	// "Method@pkgpath.Interface". A method with that name whose receiver
	// has every method of the interface is analyzed like a Receive. It
	// defaults to DefaultEntryMethods. Interface dispatch is not followed,
	// so this is how code behind an interface, such as the state handlers
	// of a state machine, is covered.
	EntryMethods []string

	// Baseline overrides BaselinePath when set.
	Baseline *Baseline

	// Recorder, when set, is told about every site that a finding in an
	// entry point depends on, whether or not the baseline covers it.
	Recorder *Recorder

	once    sync.Once
	loadErr error
}

// Recorder collects the sites that entry points depend on, across every
// package of one run. It is how a whole-program driver tells which baseline
// entries are still needed and which are stale.
type Recorder struct {
	mu     sync.Mutex
	needed map[string]map[string]int
}

// NewRecorder creates an empty recorder.
func NewRecorder() *Recorder {
	return &Recorder{needed: make(map[string]map[string]int)}
}

// record notes that a finding depends on count direct sites of the given
// kind in key.
func (r *Recorder) record(key, kind string, count int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.needed[key] == nil {
		r.needed[key] = make(map[string]int)
	}
	r.needed[key][kind] = count
}

// Needed returns the recorded direct site counts keyed by function and kind.
func (r *Recorder) Needed() map[string]map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make(map[string]map[string]int, len(r.needed))
	for key, kinds := range r.needed {
		out[key] = make(map[string]int, len(kinds))
		for kind, n := range kinds {
			out[key][kind] = n
		}
	}

	return out
}

// DefaultEntryMethods are the extra entry points checked by default: the
// state handlers of a protofsm state machine, which run inside the actor that
// drives the machine, and the dispatch of its outbox events.
var DefaultEntryMethods = []string{
	"ProcessEvent@" + DefaultProtofsmPkg + ".State",
	"Dispatch@" + DefaultProtofsmPkg + ".ActorOutboxEvent",
}

// Analyzer is the analyzer configured through its command line flags.
var Analyzer = NewAnalyzer(&Config{})

// NewAnalyzer builds an analyzer bound to cfg. The flags -actor-pkg and
// -baseline write into cfg.
func NewAnalyzer(cfg *Config) *analysis.Analyzer {
	a := &analysis.Analyzer{
		Name: "actorblock",
		Doc: "reports actor Receive turns that can block: " +
			"Future.Await on a reply, or a Tell/Ask with a " +
			"non-turn context",
		FactTypes: []analysis.Fact{
			(*MayBlock)(nil),
		},
		Flags: flag.FlagSet{},
		Run: func(pass *analysis.Pass) (any, error) {
			return run(cfg, pass)
		},
	}
	a.Flags.StringVar(
		&cfg.ActorPkg, "actor-pkg", cfg.ActorPkg,
		"import path of the actor package "+
			"(default "+DefaultActorPkg+")",
	)
	a.Flags.Func(
		"entry-methods", "comma-separated Method@pkgpath.Interface "+
			"extra entry points (default "+
			strings.Join(DefaultEntryMethods, ",")+")",
		func(v string) error {
			cfg.EntryMethods = strings.Split(v, ",")

			return nil
		},
	)
	a.Flags.StringVar(
		&cfg.BaselinePath, "baseline", cfg.BaselinePath,
		"file listing functions with tolerated legacy sites",
	)

	return a
}

// actorPkg returns the configured actor package path.
func (c *Config) actorPkg() string {
	if c.ActorPkg == "" {
		return DefaultActorPkg
	}

	return c.ActorPkg
}

// entryMethods returns the configured extra entry point specs.
func (c *Config) entryMethods() []string {
	if len(c.EntryMethods) == 0 {
		return DefaultEntryMethods
	}

	return c.EntryMethods
}

// baseline loads the baseline once.
func (c *Config) baseline() (*Baseline, error) {
	c.once.Do(func() {
		if c.Baseline != nil || c.BaselinePath == "" {
			return
		}

		c.Baseline, c.loadErr = LoadBaseline(c.BaselinePath)
	})

	return c.Baseline, c.loadErr
}

// isStd reports whether path is a standard library package, which cannot
// contain actor call sites and is skipped to keep fact computation cheap.
func isStd(path string) bool {
	first, _, _ := strings.Cut(path, "/")

	return !strings.Contains(first, ".")
}

// pkgState is the per-package analysis state.
type pkgState struct {
	cfg  *Config
	pass *analysis.Pass

	files []*ast.File

	// allowed maps a directive kind to the file lines it covers.
	allowed map[string]map[string]map[int]bool

	// params holds every parameter and result variable in the package.
	params map[*types.Var]bool

	// defs holds every right-hand side assigned to a variable. A nil
	// entry marks a definition whose value is unknown.
	defs map[*types.Var][]ast.Expr

	// direct counts the direct blocking sites per function key and kind,
	// over every body the key owns.
	direct map[string]map[string]int

	// summaries holds the sites each function of this package can reach.
	summaries map[*types.Func][]Site
}

// funcDecl pairs a declaration with its object and baseline key.
type funcDecl struct {
	decl *ast.FuncDecl
	obj  *types.Func
	key  string
}

// entry is an actor turn entry point to report on.
type entry struct {
	name string
	body ast.Node
	key  string
}

// run is the analyzer body.
func run(cfg *Config, pass *analysis.Pass) (any, error) {
	path := strings.TrimSuffix(pass.Pkg.Path(), "_test")
	if isStd(path) || path == cfg.actorPkg() {
		return nil, nil
	}

	baseline, err := cfg.baseline()
	if err != nil {
		return nil, fmt.Errorf("actorblock baseline: %w", err)
	}

	st := &pkgState{
		cfg:       cfg,
		pass:      pass,
		allowed:   make(map[string]map[string]map[int]bool),
		params:    make(map[*types.Var]bool),
		defs:      make(map[*types.Var][]ast.Expr),
		direct:    make(map[string]map[string]int),
		summaries: make(map[*types.Func][]Site),
	}
	for _, f := range pass.Files {
		name := pass.Fset.File(f.Pos()).Name()
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		st.files = append(st.files, f)
	}

	st.scanDirectives()
	st.scanVars()

	var decls []funcDecl
	for _, f := range st.files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}

			obj, ok := pass.TypesInfo.Defs[fd.Name].(*types.Func)
			if !ok {
				continue
			}

			decls = append(decls, funcDecl{
				decl: fd,
				obj:  obj,
				key:  funcKey(obj),
			})
		}
	}

	// Count the direct sites of every function before summarizing, since
	// a site carries the count of its holder. An entry that is a function
	// literal adds to the count of the declaration that contains it.
	entries := st.entries(decls)
	declBodies := make(map[ast.Node]bool, len(decls))
	for _, d := range decls {
		declBodies[d.decl.Body] = true
		st.countDirect(d.decl.Body, d.key)
	}
	for _, e := range entries {
		if !declBodies[e.body] {
			st.countDirect(e.body, e.key)
		}
	}

	// Compute summaries to a fixed point. The site sets only grow and are
	// bounded by the number of functions, so this terminates.
	for changed := true; changed; {
		changed = false
		for _, d := range decls {
			sites := st.summarize(d.decl.Body, d.key)
			if len(sites) != len(st.summaries[d.obj]) {
				st.summaries[d.obj] = sites
				changed = true
			}
		}
	}
	for _, d := range decls {
		if sites := st.summaries[d.obj]; len(sites) > 0 {
			pass.ExportObjectFact(d.obj, &MayBlock{Sites: sites})
		}
	}

	for _, e := range entries {
		st.report(e, baseline)
	}

	return nil, nil
}

// offPathCallee reports whether a function literal passed to call runs off
// the actor goroutine or is itself analyzed as an entry point, so the walk
// must not descend into it.
func (st *pkgState) offPathCallee(call *ast.CallExpr) bool {
	callee := calleeFunc(st.pass.TypesInfo, call)
	if callee == nil {
		return false
	}

	switch callee.Name() {
	case "Go", "AfterFunc":
		return true

	case "OnComplete", "ThenApply", "AskThen", "NewFunctionBehavior",
		"FunctionBehaviorFromSimple":
		return callee.Pkg() != nil &&
			callee.Pkg().Path() == st.cfg.actorPkg()
	}

	return false
}

// walk visits every call that executes on the calling goroutine in body. It
// skips go statements and the function literal arguments of off-path callees.
func (st *pkgState) walk(body ast.Node, visit func(*ast.CallExpr)) {
	var inspect func(n ast.Node) bool
	inspect = func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.GoStmt:
			return false

		case *ast.CallExpr:
			visit(n)

			if !st.offPathCallee(n) {
				return true
			}

			// Descend everywhere except into literal arguments.
			ast.Inspect(n.Fun, inspect)
			for _, arg := range n.Args {
				if _, ok := arg.(*ast.FuncLit); ok {
					continue
				}
				ast.Inspect(arg, inspect)
			}

			return false
		}

		return true
	}

	ast.Inspect(body, inspect)
}

// countDirect adds the direct blocking sites of body to the count of key.
func (st *pkgState) countDirect(body ast.Node, key string) {
	st.walk(body, func(call *ast.CallExpr) {
		for _, s := range st.callSites(call, key) {
			if strings.Contains(s.Chain, " -> ") {
				continue
			}

			if st.direct[key] == nil {
				st.direct[key] = make(map[string]int)
			}
			st.direct[key][s.Kind]++
		}
	})
}

// callSites returns the blocking sites a single call contributes, for a call
// that appears in the function with the given key.
func (st *pkgState) callSites(call *ast.CallExpr, key string) []Site {
	info := st.pass.TypesInfo
	callee := calleeFunc(info, call)
	if callee == nil {
		return nil
	}

	actorPkg := st.cfg.actorPkg()
	if callee.Pkg() != nil && callee.Pkg().Path() == actorPkg {
		sig, _ := callee.Type().(*types.Signature)
		if sig == nil || sig.Recv() == nil {
			return nil
		}

		switch callee.Name() {
		case "Await":
			if st.isAllowed(KindAwait, call) {
				return nil
			}

			return []Site{{
				Kind:  KindAwait,
				Key:   key,
				Count: max(st.direct[key][KindAwait], 1),
				Chain: "Future.Await" + st.at(call),
			}}

		case "Tell", "Ask":
			if len(call.Args) == 0 || st.isAllowed(KindSend, call) {
				return nil
			}

			if st.classify(call.Args[0], nil) != notDerived {
				return nil
			}

			return []Site{{
				Kind:  KindSend,
				Key:   key,
				Count: max(st.direct[key][KindSend], 1),
				Chain: callee.Name() + " with a non-turn " +
					"context" + st.at(call),
			}}
		}

		return nil
	}

	// Interface methods have no single target, so they are not followed.
	if sig, ok := callee.Type().(*types.Signature); ok &&
		sig.Recv() != nil {

		if types.IsInterface(sig.Recv().Type()) {
			return nil
		}
	}

	var sites []Site
	if local, ok := st.summaries[callee]; ok {
		sites = local
	} else {
		var fact MayBlock
		if st.pass.ImportObjectFact(callee, &fact) {
			sites = fact.Sites
		}
	}

	var out []Site
	for _, s := range sites {
		if st.isAllowed(s.Kind, call) {
			continue
		}

		out = append(out, Site{
			Kind:  s.Kind,
			Key:   s.Key,
			Count: s.Count,
			Chain: shortName(callee) + " -> " + s.Chain,
		})
	}

	return out
}

// at renders the position suffix of a call for a chain.
func (st *pkgState) at(call *ast.CallExpr) string {
	pos := st.pass.Fset.Position(call.Pos())

	return fmt.Sprintf(" at %s:%d", filepath.Base(pos.Filename), pos.Line)
}

// summarize returns the deduplicated sites reachable from body.
func (st *pkgState) summarize(body ast.Node, key string) []Site {
	var all []Site
	st.walk(body, func(call *ast.CallExpr) {
		all = append(all, st.callSites(call, key)...)
	})

	return dedupe(all)
}

// dedupe keeps the shortest chain per (kind, key) and the largest count, and
// sorts the result.
func dedupe(sites []Site) []Site {
	best := make(map[[2]string]Site)
	for _, s := range sites {
		id := [2]string{s.Kind, s.Key}
		cur, ok := best[id]
		if !ok {
			best[id] = s
			continue
		}

		if len(s.Chain) < len(cur.Chain) {
			cur.Chain = s.Chain
		}
		cur.Count = max(cur.Count, s.Count)
		best[id] = cur
	}

	out := make([]Site, 0, len(best))
	for _, s := range best {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key != out[j].Key {
			return out[i].Key < out[j].Key
		}

		return out[i].Kind < out[j].Kind
	})

	return out
}

// report emits the findings for one entry point.
func (st *pkgState) report(e entry, baseline *Baseline) {
	st.walk(e.body, func(call *ast.CallExpr) {
		sites := dedupe(st.callSites(call, e.key))

		var first *Site
		for i := range sites {
			s := sites[i]
			if st.cfg.Recorder != nil {
				st.cfg.Recorder.record(s.Key, s.Kind, s.Count)
			}
			if baseline.Allows(s.Key, s.Kind, s.Count) {
				continue
			}
			if first == nil {
				first = &sites[i]
			}
		}
		if first == nil {
			return
		}

		hint := "use actor.AskThen or actor.DetachAskPromise, or add " +
			"//actor:allow-await <reason>"
		if first.Kind == KindSend {
			hint = "send with the turn context or a context " +
				"derived from it, or add " +
				"//actor:allow-send <reason>"
		}

		st.pass.Reportf(
			call.Pos(),
			"%s can block its turn: %s -> %s (%s)", e.name, e.name,
			first.Chain, hint,
		)
	})
}

// calleeFunc resolves the statically known function a call invokes, or nil.
func calleeFunc(info *types.Info, call *ast.CallExpr) *types.Func {
	fn, ok := typeutil.Callee(info, call).(*types.Func)
	if !ok {
		return nil
	}

	return fn.Origin()
}

// funcKey returns the baseline key of fn: the package path and function
// name, with the receiver type name for a method, for example
// "example.com/mod/pkg.Func" or "example.com/mod/pkg.Actor.waitFor".
func funcKey(fn *types.Func) string {
	if fn.Pkg() == nil {
		return fn.Name()
	}

	return fn.Pkg().Path() + "." + localName(fn)
}

// shortName renders fn for a chain with only the package name, for example
// "pkg.Actor.waitFor".
func shortName(fn *types.Func) string {
	if fn.Pkg() == nil {
		return fn.Name()
	}

	return fn.Pkg().Name() + "." + localName(fn)
}

// localName returns "Func" or "Recv.Method" for fn.
func localName(fn *types.Func) string {
	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig.Recv() == nil {
		return fn.Name()
	}

	recv := sig.Recv().Type()
	if ptr, ok := recv.(*types.Pointer); ok {
		recv = ptr.Elem()
	}
	if named, ok := recv.(*types.Named); ok {
		return named.Obj().Name() + "." + fn.Name()
	}

	return fn.Name()
}

// isContext reports whether t is context.Context.
func isContext(t types.Type) bool {
	named, ok := t.(*types.Named)
	if !ok {
		return false
	}

	obj := named.Obj()

	return obj.Pkg() != nil && obj.Pkg().Path() == "context" &&
		obj.Name() == "Context"
}

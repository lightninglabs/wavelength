// verify-schema-registry parses the wavecli source using Go AST and
// checks that the schema registry, MCP tool definitions, and cobra
// command tree are all in sync. Run as a CI check:
//
//	go run scripts/verify-schema-registry/main.go
//
// Exit 0 = all in sync, exit 1 = drift detected.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const cliPkg = "cmd/wavecli"

func main() {
	if len(os.Args) > 1 {
		fmt.Fprintln(
			os.Stderr,
			"usage: go run scripts/verify-schema-registry/main.go",
		)
		os.Exit(2)
	}

	// Resolve CLI package directory relative to the repo root.
	repoRoot, err := findRepoRoot()
	if err != nil {
		fatalf("finding repo root: %v", err)
	}

	pkgDir := filepath.Join(repoRoot, cliPkg)

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, pkgDir, nil, 0)
	if err != nil {
		fatalf("parsing %s: %v", pkgDir, err)
	}

	pkg, ok := pkgs["main"]
	if !ok {
		fatalf("no main package found in %s", pkgDir)
	}

	// Collect method names from methodRegistry() in
	// schema_registry.go.
	schemaMethods := extractSchemaMethods(pkg)

	// Collect MCP tool names from registerMCPTools() in
	// cmd_mcp.go.
	mcpTools := extractMCPToolNames(pkg)

	// Collect cobra leaf commands (commands with RunE) from the
	// cmd_*.go files. We build the dotted path from the command
	// tree structure.
	cobraCommands := extractCobraLeafCommands(pkg)

	// Filter out meta-commands that don't correspond to RPC
	// methods (schema introspection, MCP server).
	var rpcCobraCommands []string
	for _, c := range cobraCommands {
		switch {
		case strings.HasPrefix(c, "schema"):
		case strings.HasPrefix(c, "mcp"):
		default:
			rpcCobraCommands = append(rpcCobraCommands, c)
		}
	}

	var errors []string

	// Check MCP → schema: every MCP tool must have a schema entry.
	// The reverse is not required because some schema entries are
	// CLI-only (e.g., wallet operations that handle sensitive
	// material unsuitable for MCP transport).
	errors = append(
		errors, checkSubset(
			"MCP tools", mcpTools, "schema registry", schemaMethods,
			mcpToSchema,
		)...,
	)

	// Check cobra → schema: every RPC cobra command must have a
	// schema entry.
	errors = append(
		errors, checkSubset(
			"cobra commands", rpcCobraCommands, "schema registry",
			schemaMethods, schemaToCobra,
		)...,
	)

	if len(errors) > 0 {
		fmt.Fprintln(os.Stderr,
			"Schema registry drift detected:")
		fmt.Fprintln(os.Stderr)
		for _, e := range errors {
			fmt.Fprintf(os.Stderr,
				"  - %s\n", e)
		}
		fmt.Fprintln(os.Stderr)
		fmt.Fprintf(
			os.Stderr, "Found %d issue(s). Update "+
				"schema_registry.go, cmd_mcp.go, or "+
				"cmd_*.go to fix.\n", len(errors),
		)
		os.Exit(1)
	}

	_, _ = fmt.Fprintf(
		os.Stdout, "OK: schema registry (%d methods), MCP tools "+
			"(%d), cobra commands (%d) are in sync.\n",
		len(schemaMethods), len(mcpTools), len(rpcCobraCommands),
	)
}

// mcpToSchema converts an MCP tool name (e.g. "wallet_balance") to the
// expected schema method name (e.g. "wallet.balance"). Only the first
// underscore is replaced since tool names use a single namespace level.
func mcpToSchema(tool string) string {
	return strings.Replace(tool, "_", ".", 1)
}

// schemaToCobra converts a schema method name to the expected cobra
// command path. For top-level commands like "getinfo" the cobra path
// is just "getinfo". For nested commands like "wallet.create" the
// cobra path is "wallet.create" (same format).
func schemaToCobra(method string) string {
	return method
}

// checkSubset verifies that every name in setA (after transform)
// exists in setB. It does NOT check the reverse — setB may have extra
// entries.
func checkSubset(nameA string, setA []string, nameB string, setB []string,
	transform func(string) string) []string {

	bMap := make(map[string]bool, len(setB))
	for _, b := range setB {
		bMap[b] = true
	}

	var errs []string

	for _, a := range setA {
		t := transform(a)
		if !bMap[t] {
			errs = append(
				errs, fmt.Sprintf("%s has %q but %s is "+
					"missing %q", nameA, a, nameB, t),
			)
		}
	}

	return errs
}

// extractSchemaMethods finds the methodRegistry() function and pulls
// out all string literal values of "Method" fields from the returned
// composite literals.
func extractSchemaMethods(pkg *ast.Package) []string {
	var methods []string

	for _, file := range pkg.Files {
		ast.Inspect(file, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "methodRegistry" {
				return true
			}

			// Walk the function body looking for composite
			// literals with a "Method" key.
			ast.Inspect(fn.Body, func(inner ast.Node) bool {
				kv, ok := inner.(*ast.KeyValueExpr)
				if !ok {
					return true
				}

				ident, ok := kv.Key.(*ast.Ident)
				if !ok || ident.Name != "Method" {
					return true
				}

				lit, ok := kv.Value.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}

				// Strip quotes.
				val := strings.Trim(lit.Value, `"`)
				methods = append(methods, val)

				return false
			})

			return false
		})
	}

	sort.Strings(methods)

	return methods
}

// extractMCPToolNames finds all mcp.AddTool calls and extracts the
// Name field from the &mcp.Tool{} literal.
func extractMCPToolNames(pkg *ast.Package) []string {
	var names []string

	for _, file := range pkg.Files {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			// Match mcp.AddTool calls.
			sel, ok := call.Fun.(*ast.IndexExpr)
			if ok {
				// Generic: mcp.AddTool[T](...)
				if s, ok := sel.X.(*ast.SelectorExpr); ok {
					if ident, ok := s.X.(*ast.Ident); ok {
						if ident.Name != "mcp" ||
							s.Sel.Name != "AddTool" { //nolint:ll
							return true
						}
					}
				}
			} else {
				// Non-generic fallback.
				s, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}

				ident, ok := s.X.(*ast.Ident)
				if !ok || ident.Name != "mcp" ||
					s.Sel.Name != "AddTool" {
					return true
				}
			}

			// Find the &mcp.Tool{} argument and extract
			// the Name field.
			for _, arg := range call.Args {
				name := extractToolName(arg)
				if name != "" {
					names = append(names, name)
				}
			}

			return true
		})
	}

	sort.Strings(names)

	return names
}

// extractToolName extracts the Name field value from a &mcp.Tool{}
// composite literal expression.
func extractToolName(expr ast.Expr) string {
	// Unwrap & operator.
	unary, ok := expr.(*ast.UnaryExpr)
	if ok {
		expr = unary.X
	}

	comp, ok := expr.(*ast.CompositeLit)
	if !ok {
		return ""
	}

	for _, elt := range comp.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}

		ident, ok := kv.Key.(*ast.Ident)
		if !ok || ident.Name != "Name" {
			continue
		}

		lit, ok := kv.Value.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			continue
		}

		return strings.Trim(lit.Value, `"`)
	}

	return ""
}

// extractCobraLeafCommands walks all new*Cmd() functions and extracts
// the Use field from cobra.Command literals that also have a RunE
// field. Parent commands (no RunE) are tracked for path building by
// inspecting AddCommand calls.
func extractCobraLeafCommands(pkg *ast.Package) []string {
	// First pass: map function names to their cobra Use and RunE
	// status.
	type cmdInfo struct {
		use    string
		hasRun bool
	}

	funcCmds := make(map[string]cmdInfo)
	funcChildren := make(map[string][]string)

	for _, file := range pkg.Files {
		ast.Inspect(file, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok {
				return true
			}

			name := fn.Name.Name
			if !strings.HasPrefix(name, "new") ||
				!strings.HasSuffix(name, "Cmd") {
				return true
			}

			// Extract Use and RunE from cobra.Command
			// literals in this function.
			var info cmdInfo
			var children []string

			ast.Inspect(fn.Body, func(inner ast.Node) bool {
				// Look for cobra.Command composite
				// literals.
				comp, ok := inner.(*ast.CompositeLit)
				if ok {
					use, hasRun := parseCobraLit(comp)
					if use != "" {
						info.use = use
						info.hasRun = hasRun
					}

					return true
				}

				// Look for AddCommand calls to find
				// children.
				call, ok := inner.(*ast.CallExpr)
				if !ok {
					return true
				}

				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "AddCommand" {
					return true
				}

				for _, arg := range call.Args {
					c, ok := arg.(*ast.CallExpr)
					if !ok {
						continue
					}

					if ident, ok := c.Fun.(*ast.Ident); ok {
						children = append(
							children, ident.Name,
						)
					}
				}

				return true
			})

			if info.use != "" {
				funcCmds[name] = info
			}
			if len(children) > 0 {
				funcChildren[name] = children
			}

			return true
		})
	}

	// Build the dotted command paths by walking the tree from root
	// commands.
	var leaves []string

	var walk func(funcName string, prefix string)
	walk = func(funcName string, prefix string) {
		info, ok := funcCmds[funcName]
		if !ok {
			return
		}

		path := info.use
		if prefix != "" {
			path = prefix + "." + info.use
		}

		if info.hasRun {
			leaves = append(leaves, path)
		}

		for _, child := range funcChildren[funcName] {
			walk(child, path)
		}
	}

	// Start from the known top-level commands registered in
	// newRootCmd. We find those via funcChildren.
	for _, child := range funcChildren["newRootCmd"] {
		walk(child, "")
	}

	// Also check for top-level commands that have RunE directly
	// (like getinfo).
	for _, child := range funcChildren["newRootCmd"] {
		info, ok := funcCmds[child]
		if ok && info.hasRun {
			// Already added by walk above.
			_ = info
		}
	}

	sort.Strings(leaves)

	return leaves
}

// parseCobraLit extracts the Use field value and whether RunE is set
// from a cobra.Command composite literal.
func parseCobraLit(comp *ast.CompositeLit) (string, bool) {
	// Check if this is a cobra.Command type.
	sel, ok := comp.Type.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Command" {
		return "", false
	}

	ident, ok := sel.X.(*ast.Ident)
	if !ok || ident.Name != "cobra" {
		return "", false
	}

	var use string
	var hasRun bool

	for _, elt := range comp.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}

		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}

		switch key.Name {
		case "Use":
			lit, ok := kv.Value.(*ast.BasicLit)
			if ok && lit.Kind == token.STRING {
				use = strings.Trim(lit.Value, `"`)
			}

		case "RunE":
			hasRun = true
		}
	}

	return use, hasRun
}

// findRepoRoot walks up from the current directory looking for go.mod.
func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found in any " +
				"parent directory")
		}

		dir = parent
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(2)
}

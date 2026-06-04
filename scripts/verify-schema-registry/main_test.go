package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestMcpToSchema verifies the MCP tool name → schema method name
// conversion. Only the first underscore is replaced with a dot.
func TestMcpToSchema(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{
			input: "wallet_balance",
			want:  "wallet.balance",
		},
		{
			input: "wallet_newaddress",
			want:  "wallet.newaddress",
		},
		{
			input: "vtxos_list",
			want:  "vtxos.list",
		},
		{
			input: "send_inround",
			want:  "send.inround",
		},
		{
			input: "send_oor",
			want:  "send.oor",
		},
		{
			input: "getinfo",
			want:  "getinfo",
		},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := mcpToSchema(tc.input)
			if got != tc.want {
				t.Fatalf("mcpToSchema(%q) = %q, want %q",
					tc.input, got, tc.want)
			}
		})
	}
}

// TestSchemaToCobra verifies the schema method → cobra command path
// conversion (identity function).
func TestSchemaToCobra(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{
			input: "wallet.create",
			want:  "wallet.create",
		},
		{
			input: "getinfo",
			want:  "getinfo",
		},
		{
			input: "vtxos.list",
			want:  "vtxos.list",
		},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := schemaToCobra(tc.input)
			if got != tc.want {
				t.Fatalf("schemaToCobra(%q) = %q, want %q",
					tc.input, got, tc.want)
			}
		})
	}
}

// identity returns its argument unchanged; used as the no-op transform
// for checkSubset cases that compare raw names.
func identity(s string) string { return s }

// TestCheckSubset verifies the one-directional subset check across its
// behavioral cases: full membership, a missing element, empty-set edge
// cases, and a superset setB. checkSubset must report exactly one error
// per setA element that is absent from setB after the transform.
func TestCheckSubset(t *testing.T) {
	tests := []struct {
		name      string
		setA      []string
		setB      []string
		transform func(string) string
		wantErrs  int
	}{
		{
			name: "all present after transform",
			setA: []string{
				"wallet_balance",
				"vtxos_list",
			},
			setB: []string{
				"wallet.balance", "vtxos.list", "wallet.create",
			},
			transform: mcpToSchema,
			wantErrs:  0,
		},
		{
			name: "one missing element",
			setA: []string{
				"wallet_balance", "vtxos_list", "missing_tool",
			},
			setB: []string{
				"wallet.balance",
				"vtxos.list",
			},
			transform: mcpToSchema,
			wantErrs:  1,
		},
		{
			name: "empty setA always passes",
			setA: nil,
			setB: []string{
				"x",
			},
			transform: identity,
			wantErrs:  0,
		},
		{
			name: "empty setB fails non-empty setA",
			setA: []string{
				"x",
			},
			setB:      nil,
			transform: identity,
			wantErrs:  1,
		},
		{
			name: "superset setB allowed",
			setA: []string{
				"a",
			},
			setB: []string{
				"a",
				"b",
				"c",
			},
			transform: identity,
			wantErrs:  0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errs := checkSubset(
				"A", tc.setA, "B", tc.setB, tc.transform,
			)
			if len(errs) != tc.wantErrs {
				t.Fatalf("got %d errors, want %d: %v",
					len(errs), tc.wantErrs, errs)
			}
		})
	}
}

// parseSrc parses a Go source string into an ast.Package for testing
// the AST extraction functions.
func parseSrc(t *testing.T, src string) *ast.Package {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "test.go", src, 0)
	if err != nil {
		t.Fatalf("parse source: %v", err)
	}

	return &ast.Package{
		Name: "main",
		Files: map[string]*ast.File{
			"test.go": file,
		},
	}
}

// TestExtractSchemaMethods verifies that Method fields are extracted
// from a methodRegistry() function.
func TestExtractSchemaMethods(t *testing.T) {
	src := `package main

type schemaMethod struct {
	Method string
}

func methodRegistry() []schemaMethod {
	return []schemaMethod{
		{Method: "getinfo"},
		{Method: "wallet.balance"},
		{Method: "vtxos.list"},
	}
}
`
	pkg := parseSrc(t, src)
	methods := extractSchemaMethods(pkg)

	want := []string{"getinfo", "vtxos.list", "wallet.balance"}
	if len(methods) != len(want) {
		t.Fatalf("got %d methods, want %d: %v", len(methods), len(want),
			methods)
	}

	for i, m := range methods {
		if m != want[i] {
			t.Errorf("methods[%d] = %q, want %q", i, m, want[i])
		}
	}
}

// TestExtractSchemaMethodsEmpty verifies extraction returns nil when
// methodRegistry is missing.
func TestExtractSchemaMethodsEmpty(t *testing.T) {
	src := `package main

func someOtherFunc() {}
`
	pkg := parseSrc(t, src)
	methods := extractSchemaMethods(pkg)

	if len(methods) != 0 {
		t.Fatalf("expected no methods, got: %v", methods)
	}
}

// TestExtractMCPToolNames verifies that mcp.AddTool Name fields are
// extracted correctly.
func TestExtractMCPToolNames(t *testing.T) {
	src := `package main

import "github.com/modelcontextprotocol/go-sdk/mcp"

func registerMCPTools(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "getinfo",
	}, nil)

	mcp.AddTool(s, &mcp.Tool{
		Name: "wallet_balance",
	}, nil)
}
`
	pkg := parseSrc(t, src)
	names := extractMCPToolNames(pkg)

	want := []string{"getinfo", "wallet_balance"}
	if len(names) != len(want) {
		t.Fatalf("got %d names, want %d: %v", len(names), len(want),
			names)
	}

	for i, n := range names {
		if n != want[i] {
			t.Errorf("names[%d] = %q, want %q", i, n, want[i])
		}
	}
}

// TestExtractMCPToolNamesEmpty verifies extraction returns nil when
// no mcp.AddTool calls exist.
func TestExtractMCPToolNamesEmpty(t *testing.T) {
	src := `package main

func registerMCPTools() {}
`
	pkg := parseSrc(t, src)
	names := extractMCPToolNames(pkg)

	if len(names) != 0 {
		t.Fatalf("expected no names, got: %v", names)
	}
}

// TestExtractCobraLeafCommands verifies that leaf cobra commands (those
// with RunE) are extracted with correct dotted paths.
func TestExtractCobraLeafCommands(t *testing.T) {
	src := `package main

import "github.com/spf13/cobra"

func doInfo(cmd *cobra.Command, args []string) error {
	return nil
}

func doBalance(cmd *cobra.Command, args []string) error {
	return nil
}

func doList(cmd *cobra.Command, args []string) error {
	return nil
}

func newGetInfoCmd() *cobra.Command {
	return &cobra.Command{
		Use:  "getinfo",
		RunE: doInfo,
	}
}

func newWalletBalanceCmd() *cobra.Command {
	return &cobra.Command{
		Use:  "balance",
		RunE: doBalance,
	}
}

func newWalletCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use: "wallet",
	}
	cmd.AddCommand(newWalletBalanceCmd())
	return cmd
}

func newVTXOsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:  "list",
		RunE: doList,
	}
}

func newVTXOsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use: "vtxos",
	}
	cmd.AddCommand(newVTXOsListCmd())
	return cmd
}

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use: "cli",
	}
	cmd.AddCommand(
		newGetInfoCmd(),
		newWalletCmd(),
		newVTXOsCmd(),
	)
	return cmd
}
`
	pkg := parseSrc(t, src)
	leaves := extractCobraLeafCommands(pkg)

	want := []string{
		"getinfo", "vtxos.list", "wallet.balance",
	}
	if len(leaves) != len(want) {
		t.Fatalf("got %d leaves, want %d: %v", len(leaves), len(want),
			leaves)
	}

	for i, l := range leaves {
		if l != want[i] {
			t.Errorf("leaves[%d] = %q, want %q", i, l, want[i])
		}
	}
}

// TestExtractCobraLeafCommandsSkipsParents verifies that parent
// commands without RunE are not included as leaves.
func TestExtractCobraLeafCommandsSkipsParents(t *testing.T) {
	src := `package main

import "github.com/spf13/cobra"

func doChild(cmd *cobra.Command, args []string) error {
	return nil
}

func newChildCmd() *cobra.Command {
	return &cobra.Command{
		Use:  "child",
		RunE: doChild,
	}
}

func newParentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use: "parent",
	}
	cmd.AddCommand(newChildCmd())
	return cmd
}

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use: "cli",
	}
	cmd.AddCommand(newParentCmd())
	return cmd
}
`
	pkg := parseSrc(t, src)
	leaves := extractCobraLeafCommands(pkg)

	if len(leaves) != 1 {
		t.Fatalf("got %d leaves, want 1: %v", len(leaves), leaves)
	}

	if leaves[0] != "parent.child" {
		t.Errorf("leaves[0] = %q, want %q", leaves[0], "parent.child")
	}
}

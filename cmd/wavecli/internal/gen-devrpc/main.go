package main

import (
	"bufio"
	"bytes"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"strings"

	btcwalletrpc "github.com/btcsuite/btcwallet/rpc/walletrpc"
	"github.com/lightninglabs/wavelength/rpc/swapclientrpc"
	"github.com/lightninglabs/wavelength/rpc/walletdkrpc"
	"github.com/lightninglabs/wavelength/waverpc"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const outputPath = "cmd/wavecli/waveclicommands/devrpc/" +
	"registry_generated.go"

var expectedServices = []protoreflect.FullName{
	"waverpc.DaemonService",
	"swapclientrpc.SwapClientService",
	"walletdkrpc.WalletService",
	"walletdkrpc.WalletInspectionService",
	"walletrpc.VersionService",
	"walletrpc.WalletService",
}

var protoCommentFiles = []string{
	"waverpc/daemon.proto",
	"rpc/swapclientrpc/swap_client.proto",
}

// main generates the low-level wavecli dev RPC registry.
func main() {
	services, err := collectServices()
	if err != nil {
		fatalf("%v", err)
	}

	data, err := render(services)
	if err != nil {
		fatalf("%v", err)
	}

	if err := os.MkdirAll(filepath.Dir(outputPath), 0o750); err != nil {
		fatalf("create output directory: %v", err)
	}

	if err := os.WriteFile(outputPath, data, 0o644); err != nil {
		fatalf("write %s: %v", outputPath, err)
	}
}

func collectServices() ([]serviceData, error) {
	comments, err := collectProtoComments(protoCommentFiles)
	if err != nil {
		return nil, err
	}

	files := []protoreflect.FileDescriptor{
		waverpc.File_daemon_proto,
		swapclientrpc.File_swap_client_proto,
		walletdkrpc.File_wallet_proto,
		btcwalletrpc.File_api_proto,
	}

	found := make(map[protoreflect.FullName]bool)
	var services []serviceData

	for _, file := range files {
		fileServices := file.Services()
		for i := 0; i < fileServices.Len(); i++ {
			service := fileServices.Get(i)
			if !isExpectedService(service.FullName()) {
				continue
			}

			found[service.FullName()] = true
			services = append(
				services, buildServiceData(service, comments),
			)
		}
	}

	for _, expected := range expectedServices {
		if !found[expected] {
			return nil, fmt.Errorf("missing expected service %s",
				expected)
		}
	}

	return services, nil
}

func isExpectedService(name protoreflect.FullName) bool {
	for _, expected := range expectedServices {
		if name == expected {
			return true
		}
	}

	return false
}

type serviceData struct {
	FullName string
	Aliases  []string
	Methods  []methodData
	Comments string
}

type methodData struct {
	Name            string
	Aliases         []string
	Input           string
	Output          string
	ClientStreaming bool
	ServerStreaming bool
	Comments        string
}

func buildServiceData(service protoreflect.ServiceDescriptor,
	comments map[protoreflect.FullName]string) serviceData {

	data := serviceData{
		FullName: string(service.FullName()),
		Aliases:  serviceAliases(service),
		Comments: descriptorComment(service, comments),
	}

	methods := service.Methods()
	for i := 0; i < methods.Len(); i++ {
		method := methods.Get(i)
		data.Methods = append(data.Methods, methodData{
			Name:            string(method.Name()),
			Aliases:         methodAliases(method),
			Input:           string(method.Input().FullName()),
			Output:          string(method.Output().FullName()),
			ClientStreaming: method.IsStreamingClient(),
			ServerStreaming: method.IsStreamingServer(),
			Comments:        descriptorComment(method, comments),
		})
	}

	return data
}

func serviceAliases(service protoreflect.ServiceDescriptor) []string {
	switch service.FullName() {
	case "waverpc.DaemonService":
		return []string{"daemon"}

	case "swapclientrpc.SwapClientService":
		return []string{"swapclient"}

	case "walletdkrpc.WalletService":
		return []string{"wallet"}

	case "walletdkrpc.WalletInspectionService":
		return []string{"wallet-inspection"}

	case "walletrpc.VersionService":
		return []string{"btcwallet-version"}

	case "walletrpc.WalletService":
		return []string{"btcwallet"}

	default:
		return nil
	}
}

func methodAliases(method protoreflect.MethodDescriptor) []string {
	alias := camelToKebab(normalizeInitialisms(string(method.Name())))
	if alias == string(method.Name()) {
		return nil
	}

	return []string{alias}
}

func normalizeInitialisms(value string) string {
	replacer := strings.NewReplacer(
		"VTXO", "Vtxo", "UTXO", "Utxo", "OOR", "Oor", "ECDH", "Ecdh",
		"TXID", "Txid",
	)

	return replacer.Replace(value)
}

func camelToKebab(value string) string {
	return camelToDelimited(value, '-')
}

func camelToDelimited(value string, delim rune) string {
	var out []rune
	for i, r := range value {
		if i > 0 && r >= 'A' && r <= 'Z' {
			out = append(out, delim)
		}

		out = append(out, []rune(strings.ToLower(string(r)))...)
	}

	return string(out)
}

func descriptorComment(desc protoreflect.Descriptor,
	comments map[protoreflect.FullName]string) string {

	loc := desc.ParentFile().SourceLocations().ByDescriptor(desc)
	if comment := strings.TrimSpace(loc.LeadingComments); comment != "" {
		return comment
	}

	return comments[desc.FullName()]
}

func collectProtoComments(paths []string) (map[protoreflect.FullName]string,
	error) {

	comments := make(map[protoreflect.FullName]string)
	for _, path := range paths {
		if err := collectProtoFileComments(path, comments); err != nil {
			return nil, err
		}
	}

	return comments, nil
}

func collectProtoFileComments(path string,
	comments map[protoreflect.FullName]string) error {

	resolvedPath, err := resolveRepoPath(path)
	if err != nil {
		return err
	}

	// The generator only resolves entries from protoCommentFiles, a fixed
	// allowlist of repo-local proto sources.
	file, err := os.Open(resolvedPath) //nolint:gosec
	if err != nil {
		return fmt.Errorf("open proto comments %s: %w", resolvedPath,
			err)
	}
	defer file.Close()

	var (
		pkg            string
		service        string
		serviceDepth   int
		pendingComment []string
	)

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if strings.HasPrefix(line, "//") {
			pendingComment = append(
				pendingComment,
				strings.TrimSpace(
					strings.TrimPrefix(line, "//"),
				),
			)

			continue
		}

		if line == "" {
			pendingComment = nil

			continue
		}

		switch {
		case strings.HasPrefix(line, "package "):
			pkg = strings.TrimSuffix(
				strings.TrimSpace(
					strings.TrimPrefix(line, "package "),
				),
				";",
			)
			pendingComment = nil

		case strings.HasPrefix(line, "service "):
			service = parseProtoIdent(line, "service")
			serviceDepth = countBraces(line)
			setProtoComment(
				comments, pkg, service, "", pendingComment,
			)
			pendingComment = nil

		case service != "" && strings.HasPrefix(line, "rpc "):
			method := parseProtoIdent(line, "rpc")
			setProtoComment(
				comments, pkg, service, method, pendingComment,
			)
			pendingComment = nil

		default:
			pendingComment = nil
		}

		if service != "" && !strings.HasPrefix(line, "service ") {
			serviceDepth += countBraces(line)
		}
		if serviceDepth <= 0 {
			service = ""
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan proto comments %s: %w", path, err)
	}

	return nil
}

func resolveRepoPath(path string) (string, error) {
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}

	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}

	for {
		candidate := filepath.Join(dir, path)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}

		dir = parent
	}

	return "", fmt.Errorf("resolve repo path %s", path)
}

func parseProtoIdent(line, keyword string) string {
	line = strings.TrimSpace(strings.TrimPrefix(line, keyword))
	line = strings.TrimSpace(line)

	var out strings.Builder
	for _, r := range line {
		if r == '_' || r == '.' || r >= 'A' && r <= 'Z' ||
			r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {

			out.WriteRune(r)

			continue
		}

		break
	}

	return out.String()
}

func countBraces(line string) int {
	return strings.Count(line, "{") - strings.Count(line, "}")
}

func setProtoComment(comments map[protoreflect.FullName]string, pkg, service,
	method string, lines []string) {

	comment := strings.TrimSpace(strings.Join(lines, "\n"))
	if pkg == "" || service == "" || comment == "" {
		return
	}

	name := pkg + "." + service
	if method != "" {
		name += "." + method
	}

	comments[protoreflect.FullName(name)] = comment
}

func render(services []serviceData) ([]byte, error) {
	var buf bytes.Buffer

	buf.WriteString("// Code generated by gen-devrpc. DO NOT EDIT.\n")
	buf.WriteString("package devrpc\n\n")
	buf.WriteString("import (\n")
	buf.WriteString(
		"	_ \"github.com/btcsuite/btcwallet/rpc/walletrpc\"\n",
	)
	buf.WriteString(
		"	_ \"github.com/lightninglabs/wavelength/waverpc\"\n",
	)
	buf.WriteString(
		"	_ " +
			"\"github.com/lightninglabs/wavelength/rpc/swapclient" +
			"rpc\"\n",
	)
	buf.WriteString(
		"	_ " +
			"\"github.com/lightninglabs/wavelength/rpc/walletdkrp" +
			"c\"\n",
	)
	buf.WriteString(")\n\n")

	buf.WriteString(
		"// generatedRegistry returns the generated dev RPC " +
			"service registry.\n",
	)
	buf.WriteString("func generatedRegistry() []serviceSpec {\n")
	buf.WriteString("\treturn []serviceSpec{\n")
	for _, service := range services {
		renderService(&buf, service)
	}
	buf.WriteString("\t}\n")
	buf.WriteString("}\n")

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format generated output: %w", err)
	}

	return formatted, nil
}

func renderService(buf *bytes.Buffer, service serviceData) {
	fmt.Fprintf(buf, "\t\t{\n")
	fmt.Fprintf(buf, "\t\t\tFullName: %q,\n", service.FullName)
	renderStringSlice(buf, "Aliases", service.Aliases, 3)
	fmt.Fprintf(buf, "\t\t\tComments: %q,\n", service.Comments)
	fmt.Fprintf(buf, "\t\t\tMethods: []methodSpec{\n")
	for _, method := range service.Methods {
		renderMethod(buf, method)
	}
	fmt.Fprintf(buf, "\t\t\t},\n")
	fmt.Fprintf(buf, "\t\t},\n")
}

func renderMethod(buf *bytes.Buffer, method methodData) {
	const fieldIndent = "\t\t\t\t\t"

	fmt.Fprintf(buf, "\t\t\t\t{\n")
	fmt.Fprintf(buf, "\t\t\t\t\tName: %q,\n", method.Name)
	renderStringSlice(buf, "Aliases", method.Aliases, 5)
	fmt.Fprintf(buf, "\t\t\t\t\tInput: %q,\n", method.Input)
	fmt.Fprintf(buf, "\t\t\t\t\tOutput: %q,\n", method.Output)
	if method.ClientStreaming {
		fmt.Fprintf(buf, "%sClientStreaming: true,\n", fieldIndent)
	}
	if method.ServerStreaming {
		fmt.Fprintf(buf, "%sServerStreaming: true,\n", fieldIndent)
	}
	fmt.Fprintf(buf, "%sComments: %q,\n", fieldIndent, method.Comments)
	fmt.Fprintf(buf, "\t\t\t\t},\n")
}

func renderStringSlice(buf *bytes.Buffer, name string, values []string,
	tabs int) {

	indent := strings.Repeat("\t", tabs)
	if len(values) == 0 {
		fmt.Fprintf(buf, "%s%s: nil,\n", indent, name)

		return
	}

	fmt.Fprintf(buf, "%s%s: []string{", indent, name)
	for i, value := range values {
		if i > 0 {
			buf.WriteString(", ")
		}

		fmt.Fprintf(buf, "%q", value)
	}
	buf.WriteString("},\n")
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

package gen

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
)

// TestGenerateEmitsRoutingKeys verifies the generated code uses the proto
// package name and service name as the routing service identifier.
func TestGenerateEmitsRoutingKeys(t *testing.T) {
	t.Parallel()

	req := newTestRequest("example.v1.ExampleService")
	plugin, err := protogen.Options{}.New(req)
	if err != nil {
		t.Fatalf("new plugin: %v", err)
	}

	if err := Generate(plugin, Config{}); err != nil {
		t.Fatalf("generate: %v", err)
	}

	files := responseFilesByName(plugin.Response())
	out, ok := files["test_mailboxrpc.pb.go"]
	if !ok {
		t.Fatalf("expected generated file test_mailboxrpc.pb.go")
	}

	wantService := "\"example.v1.ExampleService\""
	if !strings.Contains(out, wantService) {
		t.Fatalf("expected routing service %q in output", wantService)
	}

	wantMethod := "\"Unary\""
	if !strings.Contains(out, wantMethod) {
		t.Fatalf("expected routing method %q in output", wantMethod)
	}
}

// TestGenerateExcludeService verifies exclude_service suppresses generation for
// the specified fully-qualified proto service name.
func TestGenerateExcludeService(t *testing.T) {
	t.Parallel()

	const excluded = "example.v1.ExampleService"
	req := newTestRequest(excluded)
	req.Parameter = proto.String("paths=source_relative,exclude_service=" +
		excluded)

	var flags pluginFlagSet
	opts := protogen.Options{
		ParamFunc: flags.Set,
	}
	plugin, err := opts.New(req)
	if err != nil {
		t.Fatalf("new plugin: %v", err)
	}

	if err := Generate(plugin, flags.Config()); err != nil {
		t.Fatalf("generate: %v", err)
	}

	fileCount := len(plugin.Response().File)
	if fileCount != 0 {
		t.Fatalf("expected no files, got %d", fileCount)
	}
}

// newTestRequest returns a protoc CodeGeneratorRequest with a single proto file
// that defines one service and one request/response method.
func newTestRequest(serviceFQN string) *pluginpb.CodeGeneratorRequest {
	const inputType = ".example.v1.UnaryRequest"
	const outputType = ".example.v1.UnaryResponse"

	unaryName := proto.String("Unary")
	inType := proto.String(inputType)
	outType := proto.String(outputType)

	file := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("test.proto"),
		Package: proto.String("example.v1"),
		Syntax:  proto.String("proto3"),
		Options: &descriptorpb.FileOptions{
			GoPackage: proto.String(
				"github.com/example/testpb;testpb",
			),
		},
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("UnaryRequest"),
			},
			{
				Name: proto.String("UnaryResponse"),
			},
		},
		Service: []*descriptorpb.ServiceDescriptorProto{
			{
				Name: proto.String(lastServiceName(serviceFQN)),
				Method: []*descriptorpb.MethodDescriptorProto{
					{
						Name:       unaryName,
						InputType:  inType,
						OutputType: outType,
					},
				},
			},
		},
	}

	return &pluginpb.CodeGeneratorRequest{
		FileToGenerate: []string{
			"test.proto",
		},
		ProtoFile: []*descriptorpb.FileDescriptorProto{
			file,
		},
		Parameter: proto.String("paths=source_relative"),
	}
}

// lastServiceName returns the "<ServiceName>" portion of a proto service FQN.
func lastServiceName(serviceFQN string) string {
	if idx := strings.LastIndex(serviceFQN, "."); idx >= 0 {
		return serviceFQN[idx+1:]
	}

	return serviceFQN
}

// codeGenResponse is a short alias used to keep line lengths below 80 columns.
type codeGenResponse = pluginpb.CodeGeneratorResponse

// responseFilesByName returns the generated response files keyed by filename.
func responseFilesByName(resp *codeGenResponse) map[string]string {
	files := make(map[string]string)
	for _, f := range resp.File {
		if f.GetName() == "" {
			continue
		}
		files[f.GetName()] = f.GetContent()
	}

	return files
}

// pluginFlagSet parses protoc generator parameters for tests.
type pluginFlagSet struct {
	cfg Config
}

// Set parses a key/value parameter passed to the generator.
func (p *pluginFlagSet) Set(name, value string) error {
	if name == "exclude_service" {
		p.cfg.ExcludeService = value
		return nil
	}

	return nil
}

// Config returns the parsed generator config.
func (p *pluginFlagSet) Config() Config {
	return p.cfg
}

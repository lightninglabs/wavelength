package devrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"
)

// NewDevCmd creates the generated low-level RPC command tree.
func NewDevCmd(cfg Config) *cobra.Command {
	services, registryErr := buildRegistry()

	cmd := &cobra.Command{
		Use:   "dev [grpc_service] [call]",
		Short: "Generated low-level daemon RPC commands",
		Long: "Generated low-level access to daemon gRPC services. " +
			"Commands are built from the daemon proto descriptors.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if registryErr != nil {
				return registryErr
			}

			return printSummaryJSON(serviceSummaries(services))
		},
	}

	if registryErr != nil {
		return cmd
	}

	for _, svc := range services {
		cmd.AddCommand(newServiceCmd(cfg, svc))
	}

	return cmd
}

func newServiceCmd(cfg Config, svc rpcService) *cobra.Command {
	cmd := &cobra.Command{
		Use:     string(svc.spec.FullName),
		Aliases: svc.spec.Aliases,
		Short:   firstLine(svc.spec.Comments),
		Long:    svc.spec.Comments,
		Args:    cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return printSummaryJSON(methodSummaries(svc))
		},
	}

	if cmd.Short == "" {
		cmd.Short = "Generated RPC service"
	}

	for _, method := range svc.methods {
		cmd.AddCommand(newMethodCmd(cfg, svc.spec.FullName, method))
	}

	return cmd
}

func newMethodCmd(cfg Config, service protoreflect.FullName,
	method rpcMethod) *cobra.Command {

	binders := newFieldBinders(method.input)

	cmd := &cobra.Command{
		Use:     string(method.spec.Name),
		Aliases: method.spec.Aliases,
		Short:   firstLine(method.spec.Comments),
		Long:    method.spec.Comments,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// --describe short-circuits dispatch and dumps the
			// agent-CLI schema for this method. The flag is the
			// dev tree's equivalent of the root `schema` command:
			// it teaches an agent the exact input shape before
			// the first invocation.
			describe, _ := cmd.Flags().GetBool("describe")
			if describe {
				desc := describeMethod(
					service, method, binders,
				)

				return printMethodDescription(desc)
			}

			return runMethod(cmd, cfg, service, method, binders)
		},
	}

	if cmd.Short == "" {
		cmd.Short = "Invoke generated RPC method"
	}

	cmd.InitDefaultHelpFlag()
	cmd.Flags().SortFlags = false

	cmd.Flags().Bool("describe", false,
		"emit the JSON schema for this method's input fields "+
			"(does not dispatch the RPC)")

	for i := range binders {
		binders[i].register(cmd)
	}

	return cmd
}

func runMethod(cmd *cobra.Command, cfg Config, service protoreflect.FullName,
	method rpcMethod, binders []fieldBinder) error {

	if cfg.GetConn == nil {
		return errors.New("dev RPC command is missing connection " +
			"config")
	}
	if cfg.PrintJSON == nil {
		return errors.New("dev RPC command is missing JSON printer")
	}

	if method.spec.ClientStreaming {
		return fmt.Errorf("client-streaming RPC %s is not supported",
			method.spec.Name)
	}

	req := dynamicpb.NewMessage(method.input)
	rawJSON, _ := cmd.Flags().GetString("json")
	if rawJSON != "" {
		if err := jsonUnmarshalOpts.Unmarshal(
			[]byte(rawJSON), req,
		); err != nil {
			return fmt.Errorf("invalid --json payload: %w", err)
		}
	} else {
		if err := populateRequest(cmd, req, binders); err != nil {
			return err
		}
	}

	conn, err := cfg.GetConn(cmd)
	if err != nil {
		return err
	}
	defer conn.Close()

	ctx := cmd.Context()
	cancel := func() {}
	if cfg.RPCContext != nil {
		ctx, cancel = cfg.RPCContext(cmd)
	}
	defer cancel()

	fullMethod := "/" + string(service) + "/" + string(method.spec.Name)
	if method.spec.ServerStreaming {
		return runServerStream(
			ctx, cfg, conn, fullMethod, method.output, req,
		)
	}

	resp := dynamicpb.NewMessage(method.output)
	if err := conn.Invoke(
		ctx, fullMethod, req, resp,
	); err != nil {
		return mapRPCError(cfg, err)
	}

	return cfg.PrintJSON(resp)
}

func runServerStream(ctx context.Context, cfg Config, conn *grpc.ClientConn,
	fullMethod string, output protoreflect.MessageDescriptor,
	req proto.Message) error {

	desc := &grpc.StreamDesc{
		ServerStreams: true,
	}

	stream, err := conn.NewStream(ctx, desc, fullMethod)
	if err != nil {
		return mapRPCError(cfg, err)
	}

	if err := stream.SendMsg(req); err != nil {
		return mapRPCError(cfg, err)
	}
	if err := stream.CloseSend(); err != nil {
		return mapRPCError(cfg, err)
	}

	for {
		resp := dynamicpb.NewMessage(output)
		err := stream.RecvMsg(resp)
		switch {
		case errors.Is(err, io.EOF):
			return nil

		case err != nil:
			return mapRPCError(cfg, err)
		}

		if err := cfg.PrintJSON(resp); err != nil {
			return err
		}
	}
}

func buildRegistry() ([]rpcService, error) {
	var services []rpcService
	for _, svcSpec := range generatedRegistry() {
		service := rpcService{
			spec: svcSpec,
		}

		for _, methodSpec := range svcSpec.Methods {
			input, err := findMessage(methodSpec.Input)
			if err != nil {
				return nil, err
			}

			output, err := findMessage(methodSpec.Output)
			if err != nil {
				return nil, err
			}

			service.methods = append(service.methods, rpcMethod{
				spec:   methodSpec,
				input:  input,
				output: output,
			})
		}

		services = append(services, service)
	}

	return services, nil
}

func findMessage(name protoreflect.FullName) (protoreflect.MessageDescriptor,
	error) {

	desc, err := protoregistry.GlobalFiles.FindDescriptorByName(name)
	if err != nil {
		return nil, fmt.Errorf("find message %s: %w", name, err)
	}

	msg, ok := desc.(protoreflect.MessageDescriptor)
	if !ok {
		return nil, fmt.Errorf("descriptor %s is not a message", name)
	}

	return msg, nil
}

func mapRPCError(cfg Config, err error) error {
	if cfg.MapRPCError != nil {
		return cfg.MapRPCError(err)
	}

	return err
}

type serviceSummary struct {
	Service string   `json:"service"`
	Aliases []string `json:"aliases,omitempty"`
	Methods []string `json:"methods"`
}

type methodSummary struct {
	Method          string   `json:"method"`
	Aliases         []string `json:"aliases,omitempty"`
	RequestType     string   `json:"request_type"`
	ResponseType    string   `json:"response_type"`
	ServerStreaming bool     `json:"server_streaming,omitempty"`
}

func serviceSummaries(services []rpcService) []serviceSummary {
	summaries := make([]serviceSummary, 0, len(services))
	for _, svc := range services {
		methods := make([]string, 0, len(svc.methods))
		for _, method := range svc.methods {
			methods = append(methods, string(method.spec.Name))
		}
		sort.Strings(methods)

		summaries = append(summaries, serviceSummary{
			Service: string(svc.spec.FullName),
			Aliases: svc.spec.Aliases,
			Methods: methods,
		})
	}

	return summaries
}

func methodSummaries(svc rpcService) []methodSummary {
	summaries := make([]methodSummary, 0, len(svc.methods))
	for _, method := range svc.methods {
		summaries = append(summaries, methodSummary{
			Method:          string(method.spec.Name),
			Aliases:         method.spec.Aliases,
			RequestType:     string(method.spec.Input),
			ResponseType:    string(method.spec.Output),
			ServerStreaming: method.spec.ServerStreaming,
		})
	}

	return summaries
}

func printSummaryJSON(v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal JSON: %w", err)
	}

	fmt.Fprintln(os.Stdout, string(data))

	return nil
}

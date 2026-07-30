package devrpc

import (
	"context"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Config contains the parent CLI integrations needed by the generated dev
// RPC command tree.
type Config struct {
	// GetConn returns a daemon gRPC connection using the parent command's
	// persistent connection flags.
	GetConn func(*cobra.Command) (*grpc.ClientConn, error)

	// PrintJSON renders a proto response using the parent CLI's JSON output
	// policy.
	PrintJSON func(proto.Message) error

	// MapRPCError maps low-level gRPC errors into user-facing CLI errors.
	MapRPCError func(error) error

	// RPCContext derives the bounded context for one daemon RPC.
	RPCContext func(*cobra.Command) (context.Context, context.CancelFunc)
}

type serviceSpec struct {
	FullName protoreflect.FullName
	Aliases  []string
	Methods  []methodSpec
	Comments string
}

type methodSpec struct {
	Name            protoreflect.Name
	Aliases         []string
	Input           protoreflect.FullName
	Output          protoreflect.FullName
	ClientStreaming bool
	ServerStreaming bool
	Comments        string
}

type rpcService struct {
	spec    serviceSpec
	methods []rpcMethod
}

type rpcMethod struct {
	spec   methodSpec
	input  protoreflect.MessageDescriptor
	output protoreflect.MessageDescriptor
}

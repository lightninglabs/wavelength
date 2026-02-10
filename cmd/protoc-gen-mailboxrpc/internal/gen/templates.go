package gen

import "text/template"

// serviceData holds template data for generating mailbox RPC stubs for a
// single protobuf service.
type serviceData struct {
	// ServiceName is the Go name of the service (e.g., "ArkService").
	ServiceName string

	// ServiceFQN is the fully-qualified proto service name used as the
	// routing key (e.g., "arkrpc.ArkService").
	ServiceFQN string

	// RPCClient is the qualified Go identifier for mailboxrpc.RPCClient.
	RPCClient string

	// RPCOptions is the qualified Go identifier for mailboxrpc.RPCOptions.
	RPCOptions string

	// Router is the qualified Go identifier for mailboxrpc.Router.
	Router string

	// Context is the qualified Go identifier for context.Context.
	Context string

	// ProtoMessage is the qualified Go identifier for proto.Message.
	ProtoMessage string

	// FmtErrorf is the qualified Go identifier for fmt.Errorf.
	FmtErrorf string

	// Methods holds template data for each RPC method in the service.
	Methods []methodData
}

// methodData holds template data for a single RPC method.
type methodData struct {
	// Name is the Go method name (e.g., "GetInfo").
	Name string

	// ProtoName is the protobuf method name (e.g., "GetInfo").
	ProtoName string

	// ReqType is the qualified Go identifier for the request message.
	ReqType string

	// RespType is the qualified Go identifier for the response message.
	RespType string
}

// serviceTmpl generates typed mailbox RPC client and server stubs for a
// protobuf service. Using text/template makes the output shape directly
// visible compared to concatenating g.P() calls.
var serviceTmpl = template.Must(
	template.New("service").Parse(serviceRawTemplate),
)

// serviceRawTemplate is the Go text/template that emits a typed mailbox RPC
// client struct, server interface, router registration helper, and per-method
// client wrappers for a single protobuf service.
const serviceRawTemplate = `// {{.ServiceName}}MailboxClient is a typed mailbox RPC client for {{.ServiceName}}.
type {{.ServiceName}}MailboxClient struct {
	// C is the underlying RPC-over-mailbox runtime client.
	C {{.RPCClient}}
}

// New{{.ServiceName}}MailboxClient creates a typed mailbox client.
func New{{.ServiceName}}MailboxClient(c {{.RPCClient}}) *{{.ServiceName}}MailboxClient {
	return &{{.ServiceName}}MailboxClient{
		C: c,
	}
}

// {{.ServiceName}}MailboxServer is the mailbox server interface for {{.ServiceName}}.
type {{.ServiceName}}MailboxServer interface {
{{- range .Methods}}
	// {{.Name}} handles {{.ProtoName}}.
	{{.Name}}(ctx {{$.Context}}, req *{{.ReqType}}) (*{{.RespType}}, error)
{{- end}}
}

// Register{{.ServiceName}}MailboxServer registers handlers for {{.ServiceName}}.
func Register{{.ServiceName}}MailboxServer(r {{.Router}}, impl {{.ServiceName}}MailboxServer) {
{{- range .Methods}}
	r.Handle("{{$.ServiceFQN}}", "{{.ProtoName}}", func() {{$.ProtoMessage}} {
		return &{{.ReqType}}{}
	}, func(ctx {{$.Context}}, msg {{$.ProtoMessage}}) ({{$.ProtoMessage}}, error) {
		req, ok := msg.(*{{.ReqType}})
		if !ok {
			return nil, {{$.FmtErrorf}}("unexpected request type: %T", msg)
		}

		return impl.{{.Name}}(ctx, req)
	})
{{- end}}
}
{{range .Methods}}
// {{.Name}} calls the {{.ProtoName}} RPC.
func (c *{{$.ServiceName}}MailboxClient) {{.Name}}(ctx {{$.Context}}, req *{{.ReqType}}, opts ...{{$.RPCOptions}}) (*{{.RespType}}, error) {
	var opt {{$.RPCOptions}}
	if len(opts) > 0 {
		opt = opts[0]
	}

	correlationID, _, err := c.C.SendRPC(ctx, "{{$.ServiceFQN}}", "{{.ProtoName}}", req, opt)
	if err != nil {
		return nil, err
	}

	resp := new({{.RespType}})
	if err := c.C.AwaitRPC(ctx, correlationID, resp); err != nil {
		return nil, err
	}

	return resp, nil
}
{{end}}`

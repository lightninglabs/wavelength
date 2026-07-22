package devrpc

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// methodDescription is the agent-CLI schema dump for a single
// generated RPC method. Fields are populated from the proto
// descriptor so a `dev <svc> <method> --describe` invocation can
// teach an agent the exact input shape without it having to grep
// `.proto` files. ServerStreaming is included so an agent knows
// whether to expect a single response or a stream.
type methodDescription struct {
	Method          string             `json:"method"`
	Service         string             `json:"service"`
	RequestType     string             `json:"request_type"`
	ResponseType    string             `json:"response_type"`
	ServerStreaming bool               `json:"server_streaming"`
	Fields          []fieldDescription `json:"fields"`
}

// fieldDescription is a flat record describing one (possibly nested)
// flag the generated CLI exposes for a method. Path mirrors the
// dotted flag name (e.g. `recipient.address`), Type echoes the proto
// kind ("string", "bytes", "enum", ...), and OneofGroup captures the
// proto oneof name when the field participates in one — agents lean
// on the oneof grouping to know which flag combinations conflict.
type fieldDescription struct {
	Path        string   `json:"path"`
	Type        string   `json:"type"`
	Repeated    bool     `json:"repeated,omitempty"`
	OneofGroup  string   `json:"oneof_group,omitempty"`
	EnumValues  []string `json:"enum_values,omitempty"`
	Description string   `json:"description,omitempty"`
}

// describeMethod builds the JSON schema for a method from its proto
// descriptor and the existing fieldBinder list. Reusing the binder
// list (instead of re-walking the descriptor) keeps the schema and
// the flag surface in lockstep: a flag the binder doesn't register
// can't appear in the schema, and a flag the binder DOES register
// always does.
func describeMethod(service protoreflect.FullName, method rpcMethod,
	binders []fieldBinder) methodDescription {

	fields := make([]fieldDescription, 0, len(binders))
	for i := range binders {
		fields = append(fields, describeBinder(&binders[i]))
	}

	return methodDescription{
		Method:          string(method.spec.Name),
		Service:         string(service),
		RequestType:     string(method.input.FullName()),
		ResponseType:    string(method.output.FullName()),
		ServerStreaming: method.spec.ServerStreaming,
		Fields:          fields,
	}
}

// describeBinder projects a fieldBinder into its agent-CLI schema
// row. The proto leaf descriptor carries the kind and (for enums)
// the value list; the binder's flagName is the dotted path an agent
// will pass on the command line.
func describeBinder(b *fieldBinder) fieldDescription {
	leaf := b.leaf()
	row := fieldDescription{
		Path:        canonicalFlagPath(b.flagName),
		Type:        describeKind(leaf, b.inputKind),
		Repeated:    leaf.IsList(),
		Description: descriptorComment(leaf),
	}

	// Walk the binder path to pick up the deepest oneof group
	// the field participates in. The CLI only allows one flag per
	// oneof to be set; an agent that knows the group name can
	// pre-emptively avoid the conflict.
	for _, field := range b.path {
		if oneof := field.ContainingOneof(); oneof != nil {
			row.OneofGroup = string(oneof.Name())
		}
	}

	if leaf.Kind() == protoreflect.EnumKind {
		enum := leaf.Enum()
		values := enum.Values()
		row.EnumValues = make([]string, 0, values.Len())
		for i := 0; i < values.Len(); i++ {
			row.EnumValues = append(
				row.EnumValues,
				string(
					values.Get(i).Name(),
				),
			)
		}
	}

	return row
}

// canonicalFlagPath converts proto-style field names into the kebab-case
// spelling registered by the generated CLI's global normalization function.
func canonicalFlagPath(path string) string {
	return strings.ReplaceAll(path, "_", "-")
}

// describeKind names the field's input kind in a way that matches
// what the CLI accepts on the wire (rather than echoing the proto
// Kind name verbatim). `bytes` is exposed as `hex` because that's
// what parseScalar actually decodes; messages get the `-json`
// suffix the binder appends.
func describeKind(field protoreflect.FieldDescriptor,
	inputKind fieldInputKind) string {

	if inputKind == fieldJSON {
		return "json"
	}

	// Only the scalar kinds the dev tree binder actually exposes are
	// mapped; message / group fields are surfaced as JSON above.
	switch field.Kind() { //nolint:exhaustive
	case protoreflect.BoolKind:
		return "bool"

	case protoreflect.StringKind:
		return "string"

	case protoreflect.BytesKind:
		return "hex"

	case protoreflect.EnumKind:
		return "enum"

	case protoreflect.Int32Kind, protoreflect.Sint32Kind,
		protoreflect.Sfixed32Kind:
		return "int32"

	case protoreflect.Int64Kind, protoreflect.Sint64Kind,
		protoreflect.Sfixed64Kind:
		return "int64"

	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return "uint32"

	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return "uint64"

	case protoreflect.FloatKind:
		return "float32"

	case protoreflect.DoubleKind:
		return "float64"
	}

	return strings.ToLower(field.Kind().String())
}

// printMethodDescription writes the methodDescription to stdout as
// indented JSON. Lives next to describeMethod so the describe code
// path stays in one place.
func printMethodDescription(desc methodDescription) error {
	data, err := json.MarshalIndent(desc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal describe JSON: %w", err)
	}

	fmt.Fprintln(os.Stdout, string(data))

	return nil
}

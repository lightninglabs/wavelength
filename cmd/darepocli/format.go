package main

import (
	"encoding/json"
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// jsonUnmarshalOpts are shared options for decoding raw JSON input into
// proto messages, matching the field naming convention of the output
// marshaler.
var jsonUnmarshalOpts = protojson.UnmarshalOptions{
	DiscardUnknown: true,
}

// printJSON marshals a proto message to indented JSON and writes it to
// stdout.
func printJSON(v proto.Message) error {
	opts := protojson.MarshalOptions{
		Indent:          "  ",
		UseProtoNames:   true,
		EmitUnpopulated: true,
	}

	data, err := opts.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal JSON: %w", err)
	}

	fmt.Println(string(data))

	return nil
}

// printRawJSON marshals an arbitrary Go value to indented JSON and
// writes it to stdout. Used for non-proto types like schema output.
func printRawJSON(v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal JSON: %w", err)
	}

	fmt.Println(string(data))

	return nil
}

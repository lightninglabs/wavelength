package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// jsonMarshalOpts are the shared proto-JSON marshaling options used
// across all output paths.
var jsonMarshalOpts = protojson.MarshalOptions{
	Indent:          "  ",
	UseProtoNames:   true,
	EmitUnpopulated: true,
}

// jsonUnmarshalOpts are shared options for decoding raw JSON input into
// proto messages, matching the field naming convention of the output
// marshaler.
var jsonUnmarshalOpts = protojson.UnmarshalOptions{
	DiscardUnknown: true,
}

// printJSON marshals a proto message to indented JSON and writes it to
// stdout.
func printJSON(v proto.Message) error {
	data, err := jsonMarshalOpts.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal JSON: %w", err)
	}

	_, _ = os.Stdout.Write(append(data, '\n'))

	return nil
}

// printJSONFields marshals a proto message to JSON, then filters the
// output to only include the specified fields. For responses containing
// a top-level repeated field (e.g. "rounds"), the field mask is applied
// to each element in the array.
func printJSONFields(v proto.Message, fields []string) error {
	data, err := jsonMarshalOpts.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal JSON: %w", err)
	}

	// Parse the full JSON into a generic map.
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("unmarshal for field filter: %w", err)
	}

	// Build the allowed field set.
	allowed := make(map[string]bool, len(fields))
	for _, f := range fields {
		allowed[strings.TrimSpace(f)] = true
	}

	// Apply field mask. If the top-level value is an array of
	// objects (common for list responses), filter each element.
	filtered := filterFields(raw, allowed)

	out, err := json.MarshalIndent(filtered, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal filtered JSON: %w", err)
	}

	_, _ = os.Stdout.Write(append(out, '\n'))

	return nil
}

// filterFields applies a field mask to a JSON object. For any
// top-level value that is an array of objects, the mask is applied to
// each element in the array.
func filterFields(obj map[string]any,
	allowed map[string]bool) map[string]any {

	result := make(map[string]any)

	for key, val := range obj {
		// If this key is directly requested, keep it.
		if allowed[key] {
			result[key] = val
			continue
		}

		// If this is an array of objects (like "rounds"),
		// apply the field mask to each element and always
		// include the wrapper key.
		arr, ok := val.([]any)
		if !ok {
			continue
		}

		var filtered []any
		for _, elem := range arr {
			m, ok := elem.(map[string]any)
			if !ok {
				filtered = append(filtered, elem)
				continue
			}

			f := make(map[string]any)
			for _, field := range keysOf(allowed) {
				if v, exists := m[field]; exists {
					f[field] = v
				}
			}

			filtered = append(filtered, f)
		}

		result[key] = filtered
	}

	return result
}

// keysOf returns the keys of a map as a slice.
func keysOf(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	return keys
}

// printNDJSON marshals each element as a single-line JSON object
// (newline-delimited JSON). Useful for streaming large list responses
// into tools like jq or wc.
func printNDJSON(items []proto.Message) error {
	opts := protojson.MarshalOptions{
		UseProtoNames:   true,
		EmitUnpopulated: true,
	}

	for _, item := range items {
		data, err := opts.Marshal(item)
		if err != nil {
			return fmt.Errorf("marshal NDJSON: %w", err)
		}

		_, _ = os.Stdout.Write(append(data, '\n'))
	}

	return nil
}

// printRawJSON marshals an arbitrary Go value to indented JSON and
// writes it to stdout. Used for non-proto types like schema output.
func printRawJSON(v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal JSON: %w", err)
	}

	_, _ = os.Stdout.Write(append(data, '\n'))

	return nil
}

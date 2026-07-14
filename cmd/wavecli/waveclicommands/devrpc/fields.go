package devrpc

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

var jsonUnmarshalOpts = protojson.UnmarshalOptions{
	DiscardUnknown: true,
}

var strictUnmarshalOpts = protojson.UnmarshalOptions{}

type fieldInputKind uint8

const (
	fieldScalar fieldInputKind = iota
	fieldBool
	fieldRepeatedScalar
	fieldJSON
)

type fieldBinder struct {
	path       []protoreflect.FieldDescriptor
	flagName   string
	inputKind  fieldInputKind
	stringVal  string
	boolVal    bool
	stringVals []string
}

func newFieldBinders(msg protoreflect.MessageDescriptor) []fieldBinder {
	var binders []fieldBinder
	appendFieldBinders(&binders, nil, "", msg, nil)

	return binders
}

func appendFieldBinders(binders *[]fieldBinder,
	parentPath []protoreflect.FieldDescriptor, prefix string,
	msg protoreflect.MessageDescriptor,
	seen map[protoreflect.FullName]bool) {

	fields := msg.Fields()

	for i := 0; i < fields.Len(); i++ {
		field := fields.Get(i)
		path := appendField(parentPath, field)
		flagName := prefix + string(field.Name())

		if shouldFlattenField(field, seen) {
			nextSeen := cloneSeen(seen)
			nextSeen[field.Message().FullName()] = true
			appendFieldBinders(
				binders, path, flagName+".", field.Message(),
				nextSeen,
			)

			continue
		}

		inputKind := fieldScalar

		switch {
		case isComplexField(field):
			flagName += "-json"
			inputKind = fieldJSON

		case field.IsList():
			inputKind = fieldRepeatedScalar

		case field.Kind() == protoreflect.BoolKind:
			inputKind = fieldBool
		}

		*binders = append(*binders, fieldBinder{
			path:      path,
			flagName:  flagName,
			inputKind: inputKind,
		})
	}
}

func isComplexField(field protoreflect.FieldDescriptor) bool {
	if field.IsMap() {
		return true
	}

	return field.Kind() == protoreflect.MessageKind ||
		field.Kind() == protoreflect.GroupKind
}

func shouldFlattenField(field protoreflect.FieldDescriptor,
	seen map[protoreflect.FullName]bool) bool {

	if field.IsList() || field.IsMap() {
		return false
	}

	if field.Kind() != protoreflect.MessageKind &&
		field.Kind() != protoreflect.GroupKind {
		return false
	}

	return !seen[field.Message().FullName()]
}

func appendField(path []protoreflect.FieldDescriptor,
	field protoreflect.FieldDescriptor) []protoreflect.FieldDescriptor {

	out := make([]protoreflect.FieldDescriptor, 0, len(path)+1)
	out = append(out, path...)
	out = append(out, field)

	return out
}

func cloneSeen(
	seen map[protoreflect.FullName]bool) map[protoreflect.FullName]bool {

	out := make(map[protoreflect.FullName]bool, len(seen)+1)
	for name, ok := range seen {
		out[name] = ok
	}

	return out
}

func (b *fieldBinder) register(cmd *cobra.Command) {
	usage := flagUsage(b.leaf())

	switch b.inputKind {
	case fieldBool:
		cmd.Flags().BoolVar(&b.boolVal, b.flagName, false, usage)

	case fieldRepeatedScalar:
		cmd.Flags().StringArrayVar(
			&b.stringVals, b.flagName, nil, usage,
		)

	default:
		cmd.Flags().StringVar(&b.stringVal, b.flagName, "", usage)
	}
}

// flagUsage builds the Cobra help text for one generated field flag. Proto
// comments are useful for repo-owned descriptors, while enum values make
// comment-less external descriptors discoverable from `--help`.
func flagUsage(field protoreflect.FieldDescriptor) string {
	parts := make([]string, 0, 2)
	if comment := descriptorComment(field); comment != "" {
		parts = append(parts, comment)
	}
	if field.Kind() == protoreflect.EnumKind {
		parts = append(parts, enumUsage(field.Enum()))
	}

	return strings.Join(parts, " ")
}

// enumUsage returns the accepted symbolic names for an enum-valued flag.
func enumUsage(enum protoreflect.EnumDescriptor) string {
	values := enum.Values()
	names := make([]string, 0, values.Len())
	for i := 0; i < values.Len(); i++ {
		names = append(names, string(values.Get(i).Name()))
	}

	return "enum values: " + strings.Join(names, ", ")
}

func populateRequest(cmd *cobra.Command, msg *dynamicpb.Message,
	binders []fieldBinder) error {

	oneofs := make(map[protoreflect.FullName]oneofSelection)

	for i := range binders {
		binder := &binders[i]
		if !cmd.Flags().Changed(binder.flagName) {
			continue
		}

		if err := binder.checkOneofs(oneofs); err != nil {
			return err
		}

		if err := binder.set(msg); err != nil {
			return err
		}
	}

	return nil
}

func (b *fieldBinder) set(msg *dynamicpb.Message) error {
	parent := b.parentMessage(msg)
	leaf := b.leaf()

	switch b.inputKind {
	case fieldJSON:
		return setFieldFromJSON(parent, leaf, b.stringVal, b.flagName)

	case fieldBool:
		parent.Set(leaf, protoreflect.ValueOfBool(b.boolVal))

		return nil

	case fieldRepeatedScalar:
		list := parent.NewField(leaf).List()
		for _, raw := range b.stringVals {
			val, err := parseScalar(leaf, raw)
			if err != nil {
				return fmt.Errorf("parse --%s: %w", b.flagName,
					err)
			}

			list.Append(val)
		}

		parent.Set(leaf, protoreflect.ValueOfList(list))

		return nil

	default:
		val, err := parseScalar(leaf, b.stringVal)
		if err != nil {
			return fmt.Errorf("parse --%s: %w", b.flagName, err)
		}

		parent.Set(leaf, val)

		return nil
	}
}

func (b *fieldBinder) parentMessage(
	msg *dynamicpb.Message) protoreflect.Message {

	parent := protoreflect.Message(msg)
	for _, field := range b.path[:len(b.path)-1] {
		parent = parent.Mutable(field).Message()
	}

	return parent
}

func (b *fieldBinder) leaf() protoreflect.FieldDescriptor {
	return b.path[len(b.path)-1]
}

type oneofSelection struct {
	flag  string
	field protoreflect.FullName
}

func (b *fieldBinder) checkOneofs(
	oneofs map[protoreflect.FullName]oneofSelection) error {

	for _, field := range b.path {
		oneof := field.ContainingOneof()
		if oneof == nil {
			continue
		}

		name := oneof.FullName()
		prev, ok := oneofs[name]
		if ok && prev.field != field.FullName() {
			return fmt.Errorf("%s and %s set the same oneof %s",
				prev.flag, b.flagName, name)
		}

		oneofs[name] = oneofSelection{
			flag:  b.flagName,
			field: field.FullName(),
		}
	}

	return nil
}

func setFieldFromJSON(msg protoreflect.Message,
	field protoreflect.FieldDescriptor, raw, flagName string) error {

	if raw == "" {
		return fmt.Errorf("missing value for --%s", flagName)
	}

	data := fmt.Sprintf("{%q:%s}", string(field.Name()), raw)
	tmp := dynamicpb.NewMessage(msg.Descriptor())
	if err := strictUnmarshalOpts.Unmarshal([]byte(data), tmp); err != nil {
		return fmt.Errorf("parse --%s: %w", flagName, err)
	}

	msg.Set(field, tmp.Get(field))

	return nil
}

func parseScalar(field protoreflect.FieldDescriptor,
	raw string) (protoreflect.Value, error) {

	switch field.Kind() {
	case protoreflect.BoolKind:
		v, err := strconv.ParseBool(raw)

		return protoreflect.ValueOfBool(v), err

	case protoreflect.EnumKind:
		return parseEnum(field.Enum(), raw)

	case protoreflect.Int32Kind, protoreflect.Sint32Kind,
		protoreflect.Sfixed32Kind:

		v, err := strconv.ParseInt(raw, 10, 32)

		return protoreflect.ValueOfInt32(int32(v)), err

	case protoreflect.Int64Kind, protoreflect.Sint64Kind,
		protoreflect.Sfixed64Kind:

		v, err := strconv.ParseInt(raw, 10, 64)

		return protoreflect.ValueOfInt64(v), err

	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		v, err := strconv.ParseUint(raw, 10, 32)

		return protoreflect.ValueOfUint32(uint32(v)), err

	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		v, err := strconv.ParseUint(raw, 10, 64)

		return protoreflect.ValueOfUint64(v), err

	case protoreflect.FloatKind:
		v, err := strconv.ParseFloat(raw, 32)

		return protoreflect.ValueOfFloat32(float32(v)), err

	case protoreflect.DoubleKind:
		v, err := strconv.ParseFloat(raw, 64)

		return protoreflect.ValueOfFloat64(v), err

	case protoreflect.StringKind:
		return protoreflect.ValueOfString(raw), nil

	case protoreflect.BytesKind:
		raw = strings.TrimPrefix(raw, "0x")
		raw = strings.ReplaceAll(raw, " ", "")
		v, err := hex.DecodeString(raw)

		return protoreflect.ValueOfBytes(v), err

	default:
		return protoreflect.Value{}, fmt.Errorf("unsupported "+
			"scalar kind %s", field.Kind())
	}
}

func parseEnum(enum protoreflect.EnumDescriptor,
	raw string) (protoreflect.Value, error) {

	if number, err := strconv.ParseInt(raw, 10, 32); err == nil {
		value := protoreflect.EnumNumber(number)

		return protoreflect.ValueOfEnum(value), nil
	}

	target := normalizeAlias(raw)
	enumPrefix := normalizeAlias(string(enum.Name()))

	values := enum.Values()
	for i := 0; i < values.Len(); i++ {
		value := values.Get(i)
		aliases := []string{
			normalizeAlias(string(value.Name())),
			strings.TrimPrefix(
				normalizeAlias(
					string(
						value.Name(),
					),
				),
				enumPrefix,
			),
		}

		for _, alias := range aliases {
			if alias != "" && alias == target {
				enumValue := protoreflect.ValueOfEnum(
					value.Number(),
				)

				return enumValue, nil
			}
		}
	}

	return protoreflect.Value{}, fmt.Errorf("unknown %s value %q",
		enum.FullName(), raw)
}

func normalizeAlias(value string) string {
	value = strings.ToLower(value)
	value = strings.ReplaceAll(value, "_", "")
	value = strings.ReplaceAll(value, "-", "")
	value = strings.ReplaceAll(value, ".", "")

	return value
}

func descriptorComment(desc protoreflect.Descriptor) string {
	loc := desc.ParentFile().SourceLocations().ByDescriptor(desc)

	return strings.TrimSpace(loc.LeadingComments)
}

func firstLine(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}

	line, _, _ := strings.Cut(value, "\n")

	return strings.TrimSpace(line)
}

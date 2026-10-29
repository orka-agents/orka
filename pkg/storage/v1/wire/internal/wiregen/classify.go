package main

import (
	"fmt"
	"go/types"

	"google.golang.org/protobuf/reflect/protoreflect"
)

type convKind int

const (
	convScalar convKind = iota
	convOptional
	convBytes
	convTime
	convNullableTime
	convDuration
	convJSONAny
	convJSONMap
	convRawJSON
	convRecord
	convRecordPointer
	convList
	convScalarMap
)

// conversion is one checked pair from the type mapping table.
type conversion struct {
	kind   convKind
	goType types.Type
	// wireType is the generated Go type of a scalar wire value, or of a scalar
	// map's values.
	wireType string
	// cast is set when the Go type is not identical to wireType.
	cast   bool
	record *record
	elem   *conversion
}

const (
	timestampName    = "google.protobuf.Timestamp"
	durationName     = "google.protobuf.Duration"
	nullableTimeName = wirePackage + ".NullableTime"
	jsonValueName    = wirePackage + ".JSONValue"
)

// scalarKinds pairs Go basic kinds with wire scalar kinds and the Go type
// protoc-gen-go generates for them.
var scalarKinds = map[types.BasicKind]struct {
	wire     protoreflect.Kind
	wireType string
}{
	types.String:  {protoreflect.StringKind, "string"},
	types.Int:     {protoreflect.Int64Kind, "int64"},
	types.Int64:   {protoreflect.Int64Kind, "int64"},
	types.Int32:   {protoreflect.Int32Kind, "int32"},
	types.Uint64:  {protoreflect.Uint64Kind, "uint64"},
	types.Bool:    {protoreflect.BoolKind, "bool"},
	types.Float64: {protoreflect.DoubleKind, "float64"},
}

// classify checks that a Go type and a wire field are a pair from the
// mapping table and returns how to convert between them.
func (b *builder) classify(goType types.Type, field protoreflect.FieldDescriptor) (*conversion, error) {
	switch {
	case field.IsMap():
		if conv := classifyScalarMap(goType, field); conv != nil {
			return conv, nil
		}
		return nil, mismatch(goType, field)
	case field.IsList():
		slice, ok := goType.Underlying().(*types.Slice)
		if !ok || isBytes(goType) {
			return nil, mismatch(goType, field)
		}
		elem, err := b.classifySingular(slice.Elem(), field)
		if err != nil {
			return nil, err
		}
		switch elem.kind {
		case convScalar, convRecord, convBytes:
		default:
			return nil, fmt.Errorf("%s: a list of %s is not in the mapping table", field.FullName(), types.TypeString(slice.Elem(), nil))
		}
		return &conversion{kind: convList, goType: goType, elem: elem}, nil
	}
	return b.classifySingular(goType, field)
}

func (b *builder) classifySingular(goType types.Type, field protoreflect.FieldDescriptor) (*conversion, error) {
	if conv := classifyWellKnown(goType, field); conv != nil {
		return conv, nil
	}
	if named := b.contractRecord(goType); named != nil {
		return b.classifyRecord(named, convRecord, goType, field)
	}
	if pointer, ok := goType.(*types.Pointer); ok {
		if named := b.contractRecord(pointer.Elem()); named != nil {
			return b.classifyRecord(named, convRecordPointer, goType, field)
		}
		if conv := classifyScalar(pointer.Elem(), field, convOptional); conv != nil {
			conv.goType = goType
			return conv, nil
		}
		return nil, mismatch(goType, field)
	}
	if conv := classifyScalar(goType, field, convScalar); conv != nil {
		return conv, nil
	}
	return nil, mismatch(goType, field)
}

// wellKnown pairs Go types that have no protobuf scalar with the wire
// message that carries them.
var wellKnown = []struct {
	matches func(types.Type) bool
	wire    protoreflect.FullName
	kind    convKind
}{
	{func(t types.Type) bool { return isNamed(t, "time", "Time") }, timestampName, convTime},
	{func(t types.Type) bool { return isNamed(t, "time", "Duration") }, durationName, convDuration},
	{func(t types.Type) bool { return isNamed(t, "encoding/json", "RawMessage") }, jsonValueName, convRawJSON},
	{func(t types.Type) bool { return isPointerTo(t, "time", "Time") }, nullableTimeName, convNullableTime},
	{isEmptyInterface, jsonValueName, convJSONAny},
	{isAnyMap, jsonValueName, convJSONMap},
}

func classifyWellKnown(goType types.Type, field protoreflect.FieldDescriptor) *conversion {
	message := field.Message()
	for _, entry := range wellKnown {
		if entry.matches(goType) && message != nil && message.FullName() == entry.wire {
			return &conversion{kind: entry.kind, goType: goType}
		}
	}
	if isBytes(goType) && field.Kind() == protoreflect.BytesKind && !field.HasPresence() {
		return &conversion{kind: convBytes, goType: goType}
	}
	return nil
}

// classifyRecord pairs a contract record, or a pointer to one, with the
// wire message of the same name.
func (b *builder) classifyRecord(named *types.Named, kind convKind, goType types.Type, field protoreflect.FieldDescriptor) (*conversion, error) {
	message := field.Message()
	if message == nil || message.Name() != protoreflect.Name(named.Obj().Name()) {
		return nil, mismatch(goType, field)
	}
	rec := b.record(named.Obj().Name(), message.FullName())
	if rec == nil {
		return nil, fmt.Errorf("record %s does not map", named.Obj().Name())
	}
	return &conversion{kind: kind, goType: goType, record: rec}, nil
}

// classifyScalar pairs a Go basic type with a wire scalar. An optional
// (pointer) Go value needs a wire field with explicit presence, and a plain
// value one without it.
func classifyScalar(goType types.Type, field protoreflect.FieldDescriptor, kind convKind) *conversion {
	basic, ok := goType.Underlying().(*types.Basic)
	if !ok || field.Message() != nil || field.HasPresence() != (kind == convOptional) {
		return nil
	}
	scalar, ok := scalarKinds[basic.Kind()]
	if !ok || scalar.wire != field.Kind() {
		return nil
	}
	return &conversion{kind: kind, goType: goType, wireType: scalar.wireType, cast: !isPredeclared(goType, scalar.wireType)}
}

func mismatch(goType types.Type, field protoreflect.FieldDescriptor) error {
	wire := field.Kind().String()
	if field.Message() != nil {
		wire = string(field.Message().FullName())
	}
	if field.IsList() {
		wire = "repeated " + wire
	}
	if field.IsMap() {
		wire = "map"
	}
	return fmt.Errorf("the Go type %s and wire field %s (%s) are not a pair in the mapping table",
		types.TypeString(goType, nil), field.FullName(), wire)
}

// contractRecord returns t when it is a struct type declared by the contract.
func (b *builder) contractRecord(t types.Type) *types.Named {
	named, ok := t.(*types.Named)
	if !ok || named.Obj().Pkg() != b.contract {
		return nil
	}
	if _, ok := named.Underlying().(*types.Struct); !ok {
		return nil
	}
	return named
}

// isNamed reports whether t is the type path.name, declared as a defined
// type or as an alias. encoding/json.RawMessage is an alias in some Go
// releases.
func isNamed(t types.Type, path, name string) bool {
	if alias, ok := t.(*types.Alias); ok {
		object := alias.Obj()
		if object.Pkg() != nil && object.Pkg().Path() == path && object.Name() == name {
			return true
		}
		t = types.Unalias(t)
	}
	named, ok := t.(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == path && named.Obj().Name() == name
}

func isPointerTo(t types.Type, path, name string) bool {
	pointer, ok := t.(*types.Pointer)
	return ok && isNamed(pointer.Elem(), path, name)
}

func isEmptyInterface(t types.Type) bool {
	iface, ok := t.(*types.Interface)
	if alias, isAlias := t.(*types.Alias); isAlias {
		iface, ok = types.Unalias(alias).(*types.Interface)
	}
	return ok && iface.Empty()
}

func isAnyMap(t types.Type) bool {
	m, ok := types.Unalias(t).(*types.Map)
	if !ok {
		return false
	}
	key, ok := m.Key().(*types.Basic)
	return ok && key.Kind() == types.String && isEmptyInterface(types.Unalias(m.Elem()))
}

// classifyScalarMap pairs a Go map from string to a predeclared scalar type
// with a wire map from string to the scalar protoc-gen-go generates as that
// same Go type, such as map[string]int64 with map<string, int64>.
func classifyScalarMap(goType types.Type, field protoreflect.FieldDescriptor) *conversion {
	m, ok := goType.Underlying().(*types.Map)
	if !ok || field.MapKey().Kind() != protoreflect.StringKind || field.MapValue().Message() != nil {
		return nil
	}
	key, keyOK := m.Key().(*types.Basic)
	value, valueOK := m.Elem().(*types.Basic)
	if !keyOK || !valueOK || key.Kind() != types.String {
		return nil
	}
	scalar, ok := scalarKinds[value.Kind()]
	if !ok || scalar.wire != field.MapValue().Kind() || !isPredeclared(value, scalar.wireType) {
		return nil
	}
	return &conversion{kind: convScalarMap, goType: goType, wireType: scalar.wireType}
}

func isBytes(t types.Type) bool {
	slice, ok := t.Underlying().(*types.Slice)
	if !ok {
		return false
	}
	basic, ok := slice.Elem().(*types.Basic)
	return ok && basic.Kind() == types.Byte
}

// isPredeclared reports whether t is exactly the predeclared type name.
func isPredeclared(t types.Type, name string) bool {
	basic, ok := t.(*types.Basic)
	return ok && basic.Name() == name
}

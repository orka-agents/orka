package main

import (
	"errors"
	"fmt"
	"go/types"
	"sort"
	"strings"
	"unicode"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/orka-agents/orka/pkg/storage/v1/wire/storagepb"
)

// model is the checked mapping between the Go contract and the wire.
type model struct {
	revision int
	services []*service
	records  []*record
}

type service struct {
	desc       protoreflect.ServiceDescriptor
	name       string
	interfaces []*types.TypeName
	contract   string // "state" or "both"
	methods    []*rpcMethod
}

type rpcMethod struct {
	service *service
	desc    protoreflect.MethodDescriptor
	name    string
	sig     *types.Signature
	kind    storagepb.MethodKind
	since   int
	params  []*binding
	results []*binding
	library []protoreflect.FieldDescriptor
}

// binding pairs one Go parameter, result or record field with its wire
// field.
type binding struct {
	goName string
	goType types.Type
	field  protoreflect.FieldDescriptor
	conv   *conversion
	since  int
}

type record struct {
	name   string
	goType *types.Named
	desc   protoreflect.MessageDescriptor
	fields []*binding
}

type builder struct {
	contract *types.Package
	revision int
	messages map[protoreflect.FullName]protoreflect.MessageDescriptor
	services []protoreflect.ServiceDescriptor
	records  map[string]*record
	problems []string
}

func (b *builder) problemf(format string, args ...any) {
	b.problems = append(b.problems, fmt.Sprintf(format, args...))
}

// buildModel checks the Go contract against the wire descriptors and returns
// the mapping, or every mismatch found.
func buildModel(contract *types.Package, files []protoreflect.FileDescriptor, revision int) (*model, error) {
	b := &builder{
		contract: contract,
		revision: revision,
		messages: map[protoreflect.FullName]protoreflect.MessageDescriptor{},
		records:  map[string]*record{},
	}
	if revision < 1 {
		b.problemf("ProtocolRevision %d must be at least 1", revision)
	}
	for _, file := range files {
		for index := 0; index < file.Messages().Len(); index++ {
			message := file.Messages().Get(index)
			b.messages[message.FullName()] = message
		}
		for index := 0; index < file.Services().Len(); index++ {
			b.services = append(b.services, file.Services().Get(index))
		}
	}
	sort.Slice(b.services, func(i, j int) bool { return b.services[i].FullName() < b.services[j].FullName() })

	m := &model{revision: revision}
	methodMessages := map[protoreflect.FullName]bool{}
	mapped := map[string]string{}
	for _, desc := range b.services {
		svc := b.buildService(desc, mapped)
		if svc == nil {
			continue
		}
		m.services = append(m.services, svc)
		for _, method := range svc.methods {
			methodMessages[method.desc.Input().FullName()] = true
			methodMessages[method.desc.Output().FullName()] = true
		}
	}

	names := make([]string, 0, len(b.messages))
	for name := range b.messages {
		names = append(names, string(name))
	}
	sort.Strings(names)
	for _, name := range names {
		message := b.messages[protoreflect.FullName(name)]
		if methodMessages[message.FullName()] || wireOnly(message) {
			continue
		}
		b.record(string(message.Name()), message.FullName())
	}
	if len(b.problems) > 0 {
		sort.Strings(b.problems)
		return nil, errors.New("the Go contract and the wire do not match:\n  " + strings.Join(b.problems, "\n  "))
	}
	recordNames := make([]string, 0, len(b.records))
	for name := range b.records {
		recordNames = append(recordNames, name)
	}
	sort.Strings(recordNames)
	for _, name := range recordNames {
		m.records = append(m.records, b.records[name])
	}
	return m, nil
}

func wireOnly(message protoreflect.MessageDescriptor) bool {
	value, _ := proto.GetExtension(message.Options(), storagepb.E_WireOnly).(bool)
	return value
}

func libraryField(field protoreflect.FieldDescriptor) bool {
	value, _ := proto.GetExtension(field.Options(), storagepb.E_Library).(bool)
	return value
}

func fieldSince(field protoreflect.FieldDescriptor) int {
	value, _ := proto.GetExtension(field.Options(), storagepb.E_Since).(int32)
	return int(value)
}

func fieldGoName(field protoreflect.FieldDescriptor) string {
	value, _ := proto.GetExtension(field.Options(), storagepb.E_GoName).(string)
	return value
}

// buildService maps one service to the Go interfaces it names. mapped
// records which service already maps each Go method.
func (b *builder) buildService(desc protoreflect.ServiceDescriptor, mapped map[string]string) *service {
	svc := &service{desc: desc, name: string(desc.Name())}
	interfaceNames, _ := proto.GetExtension(desc.Options(), storagepb.E_GoInterface).([]string)
	if len(interfaceNames) == 0 {
		b.problemf("service %s names no Go interface", desc.Name())
		return nil
	}
	goMethods := map[string]*types.Func{}
	inState, inArtifacts := true, true
	for _, name := range interfaceNames {
		object, ok := b.contract.Scope().Lookup(name).(*types.TypeName)
		if !ok {
			b.problemf("service %s names %s, which is not a contract type", desc.Name(), name)
			continue
		}
		iface, ok := object.Type().Underlying().(*types.Interface)
		if !ok {
			b.problemf("service %s names %s, which is not an interface", desc.Name(), name)
			continue
		}
		svc.interfaces = append(svc.interfaces, object)
		inState = inState && b.providerImplements("StateProvider", iface)
		inArtifacts = inArtifacts && b.providerImplements("ArtifactProvider", iface)
		for goMethod := range iface.Methods() {
			goMethods[goMethod.Name()] = goMethod
		}
	}
	switch {
	case inState && inArtifacts:
		svc.contract = "both"
	case inState:
		svc.contract = "state"
	default:
		b.problemf("service %s serves interfaces outside state.v1, which the server library does not serve yet", desc.Name())
	}

	rpcs := map[string]protoreflect.MethodDescriptor{}
	for index := 0; index < desc.Methods().Len(); index++ {
		rpc := desc.Methods().Get(index)
		rpcs[string(rpc.Name())] = rpc
		if _, ok := goMethods[string(rpc.Name())]; !ok {
			b.problemf("RPC %s has no method in the Go interfaces of service %s", rpc.FullName(), desc.Name())
		}
	}
	names := make([]string, 0, len(goMethods))
	for name := range goMethods {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		rpc, ok := rpcs[name]
		if !ok {
			b.problemf("method %s has no RPC in service %s", name, desc.Name())
			continue
		}
		if previous, ok := mapped[name]; ok {
			b.problemf("method %s is served by both %s and %s", name, previous, desc.Name())
			continue
		}
		mapped[name] = string(desc.Name())
		if method := b.buildMethod(svc, rpc, goMethods[name]); method != nil {
			svc.methods = append(svc.methods, method)
		}
	}
	return svc
}

func (b *builder) providerImplements(provider string, iface *types.Interface) bool {
	object, ok := b.contract.Scope().Lookup(provider).(*types.TypeName)
	if !ok {
		b.problemf("the contract declares no %s", provider)
		return false
	}
	return types.Implements(object.Type(), iface)
}

// buildMethod maps one RPC: parameters by name to request fields, and
// results by position to response fields.
func (b *builder) buildMethod(svc *service, rpc protoreflect.MethodDescriptor, goMethod *types.Func) *rpcMethod {
	sig := goMethod.Type().(*types.Signature)
	method := &rpcMethod{service: svc, desc: rpc, name: goMethod.Name(), sig: sig}
	where := string(rpc.FullName())
	if rpc.IsStreamingClient() || rpc.IsStreamingServer() {
		b.problemf("%s streams, which the generator does not support yet", where)
		return nil
	}
	if rpc.Input().Name() != protoreflect.Name(method.name+"Request") ||
		rpc.Output().Name() != protoreflect.Name(method.name+"Response") {
		b.problemf("%s must take %sRequest and return %sResponse", where, method.name, method.name)
		return nil
	}
	method.kind, _ = proto.GetExtension(rpc.Options(), storagepb.E_Kind).(storagepb.MethodKind)
	if method.kind != storagepb.MethodKind_METHOD_KIND_READ && method.kind != storagepb.MethodKind_METHOD_KIND_WRITE {
		b.problemf("%s declares no kind", where)
	}
	since, _ := proto.GetExtension(rpc.Options(), storagepb.E_RpcSince).(int32)
	method.since = int(since)
	if method.since < 1 || method.since > b.revision {
		b.problemf("%s has rpc_since %d outside 1 to %d", where, method.since, b.revision)
	}
	if sig.Variadic() {
		b.problemf("method %s is variadic", method.name)
		return nil
	}

	if !b.bindParams(method) || !b.bindResults(method) {
		return nil
	}
	return method
}

// bindParams maps every Go parameter after the context, by name, to a
// request field, and requires every request field to have one.
func (b *builder) bindParams(method *rpcMethod) bool {
	params := method.sig.Params()
	if params.Len() == 0 || !isContext(params.At(0).Type()) {
		b.problemf("method %s does not take a context.Context first", method.name)
		return false
	}
	request := method.desc.Input()
	claimed := map[protoreflect.FieldNumber]bool{}
	for index := 1; index < params.Len(); index++ {
		param := params.At(index)
		if param.Name() == "" || param.Name() == "_" {
			b.problemf("method %s has an unnamed parameter %d", method.name, index)
			continue
		}
		field := b.fieldFor(request, param.Name())
		if field == nil {
			b.problemf("parameter %s of %s has no field in %s", param.Name(), method.name, request.Name())
			continue
		}
		claimed[field.Number()] = true
		conv, err := b.classify(param.Type(), field)
		if err != nil {
			b.problemf("parameter %s of %s: %v", param.Name(), method.name, err)
			continue
		}
		bound := &binding{goName: param.Name(), goType: param.Type(), field: field, conv: conv, since: fieldSince(field)}
		if libraryField(field) {
			b.problemf("request field %s cannot be a library field", field.FullName())
		}
		if bound.since != 0 && (bound.since <= method.since || bound.since > b.revision) {
			b.problemf("request field %s has since %d; it must be after the RPC's revision %d and at most %d",
				field.FullName(), bound.since, method.since, b.revision)
		}
		method.params = append(method.params, bound)
	}
	for index := 0; index < request.Fields().Len(); index++ {
		if field := request.Fields().Get(index); !claimed[field.Number()] {
			b.problemf("request field %s has no parameter in method %s", field.FullName(), method.name)
		}
	}
	return true
}

// bindResults maps the Go results before the final error, by position, to
// the response fields in field-number order. Library fields have no Go
// counterpart.
func (b *builder) bindResults(method *rpcMethod) bool {
	results := method.sig.Results()
	if results.Len() == 0 || !isError(results.At(results.Len()-1).Type()) {
		b.problemf("method %s does not return an error last", method.name)
		return false
	}
	var responseFields []protoreflect.FieldDescriptor
	response := method.desc.Output()
	for index := 0; index < response.Fields().Len(); index++ {
		field := response.Fields().Get(index)
		if fieldSince(field) != 0 {
			b.problemf("response field %s cannot declare since", field.FullName())
		}
		if libraryField(field) {
			method.library = append(method.library, field)
			continue
		}
		responseFields = append(responseFields, field)
	}
	sort.Slice(responseFields, func(i, j int) bool { return responseFields[i].Number() < responseFields[j].Number() })
	if len(responseFields) != results.Len()-1 {
		b.problemf("method %s returns %d values but %s has %d fields", method.name, results.Len()-1, response.Name(), len(responseFields))
		return false
	}
	for index, field := range responseFields {
		result := results.At(index)
		conv, err := b.classify(result.Type(), field)
		if err != nil {
			b.problemf("result %d of %s: %v", index, method.name, err)
			continue
		}
		method.results = append(method.results, &binding{goName: fmt.Sprintf("r%d", index), goType: result.Type(), field: field, conv: conv})
	}
	return true
}

// fieldFor finds the wire field for a Go name: the field whose go_name
// option names it, or else the field named its snake case.
func (b *builder) fieldFor(message protoreflect.MessageDescriptor, goName string) protoreflect.FieldDescriptor {
	var byName protoreflect.FieldDescriptor
	for index := 0; index < message.Fields().Len(); index++ {
		field := message.Fields().Get(index)
		if explicit := fieldGoName(field); explicit != "" {
			if explicit == goName {
				return field
			}
			continue
		}
		if string(field.Name()) == snakeCase(goName) {
			byName = field
		}
	}
	return byName
}

// record maps a contract record to its wire message, once.
func (b *builder) record(name string, wireName protoreflect.FullName) *record {
	if existing, ok := b.records[name]; ok {
		return existing
	}
	message, ok := b.messages[wireName]
	if !ok {
		b.problemf("record %s has no wire message %s", name, wireName)
		return nil
	}
	if wireOnly(message) {
		b.problemf("record %s maps to wire_only message %s", name, wireName)
		return nil
	}
	object, ok := b.contract.Scope().Lookup(name).(*types.TypeName)
	if !ok {
		b.problemf("wire message %s has no Go contract record", wireName)
		return nil
	}
	named, ok := object.Type().(*types.Named)
	if !ok {
		b.problemf("wire message %s maps to %s, which is not a named type", wireName, name)
		return nil
	}
	structType, ok := named.Underlying().(*types.Struct)
	if !ok {
		b.problemf("wire message %s maps to %s, which is not a struct", wireName, name)
		return nil
	}
	rec := &record{name: name, goType: named, desc: message}
	b.records[name] = rec
	claimed := map[protoreflect.FieldNumber]bool{}
	for index := 0; index < structType.NumFields(); index++ {
		goField := structType.Field(index)
		if !goField.Exported() || goField.Embedded() {
			b.problemf("field %s.%s cannot cross the wire: only exported, named fields can", name, goField.Name())
			continue
		}
		field := b.fieldFor(message, goField.Name())
		if field == nil {
			b.problemf("field %s.%s (json %q) has no field in wire message %s",
				name, goField.Name(), jsonTag(structType.Tag(index)), message.Name())
			continue
		}
		claimed[field.Number()] = true
		if fieldSince(field) != 0 || libraryField(field) {
			b.problemf("record field %s cannot declare since or library", field.FullName())
		}
		conv, err := b.classify(goField.Type(), field)
		if err != nil {
			b.problemf("field %s.%s: %v", name, goField.Name(), err)
			continue
		}
		rec.fields = append(rec.fields, &binding{goName: goField.Name(), goType: goField.Type(), field: field, conv: conv})
	}
	for index := 0; index < message.Fields().Len(); index++ {
		if field := message.Fields().Get(index); !claimed[field.Number()] {
			b.problemf("wire field %s has no field in Go record %s", field.FullName(), name)
		}
	}
	return rec
}

func jsonTag(tag string) string {
	value, ok := lookupTag(tag, "json")
	if !ok {
		return ""
	}
	return value
}

func lookupTag(tag, key string) (string, bool) {
	for part := range strings.FieldsSeq(tag) {
		name, value, ok := strings.Cut(part, ":")
		if ok && name == key {
			return strings.Trim(value, `"`), true
		}
	}
	return "", false
}

func isContext(t types.Type) bool {
	named, ok := t.(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == "context" && named.Obj().Name() == "Context"
}

func isError(t types.Type) bool {
	return types.Identical(t, types.Universe.Lookup("error").Type())
}

// snakeCase converts a Go identifier to a wire field name: DatasetID becomes
// dataset_id and taskName becomes task_name.
func snakeCase(name string) string {
	runes := []rune(name)
	var builder strings.Builder
	for index, r := range runes {
		if unicode.IsUpper(r) {
			if index > 0 {
				previous := runes[index-1]
				nextLower := index+1 < len(runes) && unicode.IsLower(runes[index+1])
				if unicode.IsLower(previous) || unicode.IsDigit(previous) || (unicode.IsUpper(previous) && nextLower) {
					builder.WriteByte('_')
				}
			}
			builder.WriteRune(unicode.ToLower(r))
			continue
		}
		builder.WriteRune(r)
	}
	return builder.String()
}

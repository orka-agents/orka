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

// Contracts a service serves: one contract part, or both for the Dataset
// service every provider serves and for an optional service either part's
// provider may serve.
const (
	contractState     = "state"
	contractArtifacts = "artifacts"
	contractBoth      = "both"
)

type service struct {
	desc       protoreflect.ServiceDescriptor
	name       string
	interfaces []*types.TypeName
	contract   string
	// optional is set for a service whose interfaces neither contract part
	// requires yet; a server serves it only from a provider that implements
	// them.
	optional bool
	methods  []*rpcMethod
}

// streamShape is how an RPC carries its request and response.
type streamShape int

const (
	// streamNone is a unary RPC.
	streamNone streamShape = iota
	// streamUpload is a client-streaming RPC whose request carries a blob.
	streamUpload
	// streamDownload is a server-streaming RPC whose response carries a blob.
	streamDownload
	// streamSnapshot is a server-streaming read whose response list crosses
	// in batches.
	streamSnapshot
)

// Call classes, named as the wire package's Class constants.
const (
	classHealth  = "ClassHealth"
	classControl = "ClassControl"
	classBulk    = "ClassBulk"
)

type rpcMethod struct {
	service *service
	desc    protoreflect.MethodDescriptor
	name    string
	sig     *types.Signature
	kind    storagepb.MethodKind
	since   int
	stream  streamShape
	class   string
	// batched is the path to the response list a snapshot RPC carries in
	// batches.
	batched []protoreflect.FieldDescriptor
	// inline is set when the method's only parameter is a contract record
	// named <Method>Request; that record's wire message is the request.
	inline  *record
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
	streamed := map[protoreflect.FullName]bool{}
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
			if method.stream != streamNone {
				for _, message := range []protoreflect.MessageDescriptor{method.desc.Input(), method.desc.Output()} {
					for _, path := range blobPaths(message, nil, map[protoreflect.FullName]bool{}) {
						streamed[path.fields[len(path.fields)-1].FullName()] = true
					}
				}
			}
		}
	}

	names := make([]string, 0, len(b.messages))
	for name := range b.messages {
		names = append(names, string(name))
	}
	sort.Strings(names)
	for _, name := range names {
		message := b.messages[protoreflect.FullName(name)]
		for index := 0; index < message.Fields().Len(); index++ {
			if field := message.Fields().Get(index); blobField(field) && !streamed[field.FullName()] {
				b.problemf("blob field %s is not the blob of a streaming RPC", field.FullName())
			}
		}
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

func blobField(field protoreflect.FieldDescriptor) bool {
	value, _ := proto.GetExtension(field.Options(), storagepb.E_Blob).(bool)
	return value
}

func optionalService(desc protoreflect.ServiceDescriptor) bool {
	value, _ := proto.GetExtension(desc.Options(), storagepb.E_Optional).(bool)
	return value
}

func batchedOption(rpc protoreflect.MethodDescriptor) string {
	value, _ := proto.GetExtension(rpc.Options(), storagepb.E_Batched).(string)
	return value
}

func fieldGoName(field protoreflect.FieldDescriptor) string {
	value, _ := proto.GetExtension(field.Options(), storagepb.E_GoName).(string)
	return value
}

// buildService maps one service to the Go interfaces it names. mapped
// records which service already maps each Go method.
func (b *builder) buildService(desc protoreflect.ServiceDescriptor, mapped map[string]string) *service {
	svc := &service{desc: desc, name: string(desc.Name()), optional: optionalService(desc)}
	interfaceNames, _ := proto.GetExtension(desc.Options(), storagepb.E_GoInterface).([]string)
	if len(interfaceNames) == 0 {
		b.problemf("service %s names no Go interface", desc.Name())
		return nil
	}
	goMethods := map[string]*types.Func{}
	inState, inArtifacts := true, true
	inEither := false
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
		state, artifacts := b.providerImplements("StateProvider", iface), b.providerImplements("ArtifactProvider", iface)
		inState = inState && state
		inArtifacts = inArtifacts && artifacts
		inEither = inEither || state || artifacts
		for goMethod := range iface.Methods() {
			goMethods[goMethod.Name()] = goMethod
		}
	}
	switch {
	case svc.optional && inEither:
		b.problemf("optional service %s names an interface a contract part requires; a service of required "+
			"interfaces is not optional", desc.Name())
	case svc.optional:
		svc.contract = contractBoth
	case inState && inArtifacts:
		svc.contract = contractBoth
	case inState:
		svc.contract = contractState
	case inArtifacts:
		svc.contract = contractArtifacts
	default:
		b.problemf("service %s mixes state.v1 and artifacts.v1 interfaces, or names interfaces neither part requires "+
			"without being optional; a service serves one contract part, both, or is optional", desc.Name())
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
	switch {
	case rpc.IsStreamingClient() && rpc.IsStreamingServer():
		b.problemf("%s streams in both directions, which the wire does not use", where)
		return nil
	case rpc.IsStreamingClient():
		method.stream = streamUpload
	case rpc.IsStreamingServer() && batchedOption(rpc) != "":
		method.stream = streamSnapshot
	case rpc.IsStreamingServer():
		method.stream = streamDownload
	}
	if batchedOption(rpc) != "" && method.stream != streamSnapshot {
		b.problemf("%s declares batched but does not stream only its response", where)
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
	if method.stream == streamUpload && method.kind != storagepb.MethodKind_METHOD_KIND_WRITE {
		b.problemf("%s streams its request, so it must be a write", where)
	}
	if (method.stream == streamDownload || method.stream == streamSnapshot) && method.kind != storagepb.MethodKind_METHOD_KIND_READ {
		b.problemf("%s streams its response, so it must be a read", where)
	}
	switch {
	case svc.contract == contractBoth && !svc.optional:
		method.class = classHealth
		if method.stream != streamNone {
			b.problemf("%s is a health call, which cannot stream", where)
		}
	case method.stream != streamNone:
		method.class = classBulk
	default:
		method.class = classControl
	}
	if method.stream == streamSnapshot {
		method.batched = b.batchedPath(rpc)
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

	b.checkBlobs(method)
	if !b.bindParams(method) || !b.bindResults(method) {
		return nil
	}
	return method
}

// checkBlobs requires a streaming RPC to carry exactly one blob field in the
// direction it streams and none in the other, and a unary RPC to carry none.
func (b *builder) checkBlobs(method *rpcMethod) {
	where := string(method.desc.FullName())
	request := blobPaths(method.desc.Input(), nil, map[protoreflect.FullName]bool{})
	response := blobPaths(method.desc.Output(), nil, map[protoreflect.FullName]bool{})
	for _, path := range append(append([]blobPath(nil), request...), response...) {
		if path.problem != "" {
			b.problemf("%s: %s", where, path.problem)
		}
	}
	want := map[streamShape][2]int{streamNone: {0, 0}, streamUpload: {1, 0}, streamDownload: {0, 1}, streamSnapshot: {0, 0}}[method.stream]
	if len(request) != want[0] || len(response) != want[1] {
		b.problemf("%s has %d blob fields in its request and %d in its response; a unary RPC and a batched "+
			"read have none, and a blob RPC exactly one, in the direction it streams", where, len(request), len(response))
	}
}

// batchedPath resolves a snapshot RPC's batched option: field names from the
// response message, through singular message fields outside any oneof, to a
// repeated message field.
func (b *builder) batchedPath(rpc protoreflect.MethodDescriptor) []protoreflect.FieldDescriptor {
	where, option := string(rpc.FullName()), batchedOption(rpc)
	message := rpc.Output()
	names := strings.Split(option, ".")
	path := make([]protoreflect.FieldDescriptor, 0, len(names))
	for index, name := range names {
		field := message.Fields().ByName(protoreflect.Name(name))
		if field == nil {
			b.problemf("%s batches %q, but %s has no field %q", where, option, message.Name(), name)
			return nil
		}
		path = append(path, field)
		last := index == len(names)-1
		if field.ContainingOneof() != nil || field.IsMap() || field.Message() == nil || field.IsList() != last {
			b.problemf("%s batches %q, which must reach a repeated message field through singular message fields "+
				"outside any oneof", where, option)
			return nil
		}
		message = field.Message()
	}
	return path
}

// blobPath is the chain of fields from a request or response message to one
// blob field, and any problem with that blob field.
type blobPath struct {
	fields  []protoreflect.FieldDescriptor
	problem string
}

// blobPaths finds the blob fields in message and in the messages it holds.
// A blob is a singular bytes field outside any oneof, reached through
// singular message fields, so a framed message names exactly one place for
// its blob.
func blobPaths(message protoreflect.MessageDescriptor, prefix []protoreflect.FieldDescriptor,
	visiting map[protoreflect.FullName]bool) []blobPath {
	if visiting[message.FullName()] {
		return nil
	}
	visiting[message.FullName()] = true
	defer delete(visiting, message.FullName())
	var paths []blobPath
	for index := 0; index < message.Fields().Len(); index++ {
		field := message.Fields().Get(index)
		path := append(append([]protoreflect.FieldDescriptor(nil), prefix...), field)
		if blobField(field) {
			found := blobPath{fields: path}
			if field.Kind() != protoreflect.BytesKind || field.IsList() || field.IsMap() || field.ContainingOneof() != nil {
				found.problem = fmt.Sprintf("blob field %s must be a singular bytes field outside any oneof", field.FullName())
			}
			paths = append(paths, found)
			continue
		}
		nested := field.Message()
		if field.IsMap() {
			nested = field.MapValue().Message()
		}
		if nested == nil {
			continue
		}
		for _, inner := range blobPaths(nested, path, visiting) {
			if inner.problem == "" && (field.IsList() || field.IsMap() || field.ContainingOneof() != nil) {
				inner.problem = fmt.Sprintf("blob field %s is inside list, map or oneof field %s",
					inner.fields[len(inner.fields)-1].FullName(), field.FullName())
			}
			paths = append(paths, inner)
		}
	}
	return paths
}

// inlineRecord returns the contract record a method takes as its only
// parameter when the record is named <Method>Request. Its wire message is
// then the RPC's request message, since the two would otherwise share a name.
func (b *builder) inlineRecord(method *rpcMethod) *types.Named {
	params := method.sig.Params()
	if params.Len() != 2 {
		return nil
	}
	named := b.contractRecord(params.At(1).Type())
	if named == nil || named.Obj().Name() != method.name+"Request" {
		return nil
	}
	return named
}

// bindParams maps every Go parameter after the context, by name, to a
// request field, and requires every request field to have one. A method that
// takes an inline record maps that record to the whole request message.
func (b *builder) bindParams(method *rpcMethod) bool {
	params := method.sig.Params()
	if params.Len() == 0 || !isContext(params.At(0).Type()) {
		b.problemf("method %s does not take a context.Context first", method.name)
		return false
	}
	request := method.desc.Input()
	if named := b.inlineRecord(method); named != nil {
		rec := b.record(named.Obj().Name(), request.FullName())
		if rec == nil {
			return false
		}
		method.inline = rec
		param := params.At(1)
		method.params = append(method.params, &binding{goName: param.Name(), goType: param.Type(),
			conv: &conversion{kind: convRecord, goType: param.Type(), record: rec}})
		return true
	}
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
		if !goField.Exported() || (goField.Embedded() && b.contractRecord(goField.Type()) == nil) {
			b.problemf("field %s.%s cannot cross the wire: only exported fields, and embedded contract records, can",
				name, goField.Name())
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

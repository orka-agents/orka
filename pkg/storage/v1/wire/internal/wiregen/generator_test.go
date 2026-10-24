package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/orka-agents/orka/pkg/storage/v1/wire/storagepb"
)

const (
	testContractDir = "../../.."
	testWireDir     = "../.."
)

func realModel(t *testing.T) *model {
	t.Helper()
	contract, err := loadContract(testContractDir)
	if err != nil {
		t.Fatal(err)
	}
	m, err := buildModel(contract, wireFiles(), 1)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// The committed generated files match what the generator writes now.
func TestGeneratedFilesAreCurrent(t *testing.T) {
	revision, err := loadProtocolRevision(filepath.Join(testWireDir, "protocol.go"))
	if err != nil {
		t.Fatal(err)
	}
	contract, err := loadContract(testContractDir)
	if err != nil {
		t.Fatal(err)
	}
	m, err := buildModel(contract, wireFiles(), revision)
	if err != nil {
		t.Fatal(err)
	}
	outputs, err := render(m)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range outputs {
		got, err := os.ReadFile(filepath.Join(testWireDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s is stale; run make generate-storage", name)
		}
	}
}

// expectProblem builds the model and requires a failure naming want.
func expectProblem(t *testing.T, contractDir string, files []protoreflect.FileDescriptor, want ...string) {
	t.Helper()
	contract, err := loadContract(contractDir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = buildModel(contract, files, 1)
	if err == nil {
		t.Fatalf("generation succeeded; want a failure naming %q", want)
	}
	for _, text := range want {
		if !strings.Contains(err.Error(), text) {
			t.Errorf("generation error does not name %q:\n%v", text, err)
		}
	}
}

// editedContract copies the contract sources and replaces old with new in
// the file that contains old.
func editedContract(t *testing.T, old, replacement string) string {
	t.Helper()
	dir := t.TempDir()
	names, err := filepath.Glob(filepath.Join(testContractDir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	replaced := false
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		content, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(content), old) {
			content, replaced = []byte(strings.Replace(string(content), old, replacement, 1)), true
		}
		if err := os.WriteFile(filepath.Join(dir, filepath.Base(name)), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if !replaced {
		t.Fatalf("no contract source contains %q", old)
	}
	return dir
}

// editedWire rebuilds the wire descriptors after edit changes them.
func editedWire(t *testing.T, edit func(map[string]*descriptorpb.FileDescriptorProto)) []protoreflect.FileDescriptor {
	t.Helper()
	protos := map[string]*descriptorpb.FileDescriptorProto{}
	set := &descriptorpb.FileDescriptorSet{}
	seen := map[string]bool{}
	var add func(file protoreflect.FileDescriptor)
	add = func(file protoreflect.FileDescriptor) {
		if seen[file.Path()] {
			return
		}
		seen[file.Path()] = true
		for index := 0; index < file.Imports().Len(); index++ {
			add(file.Imports().Get(index).FileDescriptor)
		}
		fileProto := protodesc.ToFileDescriptorProto(file)
		protos[file.Path()] = fileProto
		set.File = append(set.File, fileProto)
	}
	for _, file := range wireFiles() {
		add(file)
	}
	edit(protos)
	registry, err := protodesc.NewFiles(set)
	if err != nil {
		t.Fatal(err)
	}
	var files []protoreflect.FileDescriptor
	registry.RangeFilesByPackage(wirePackage, func(file protoreflect.FileDescriptor) bool {
		files = append(files, file)
		return true
	})
	return files
}

func message(t *testing.T, protos map[string]*descriptorpb.FileDescriptorProto, file, name string) *descriptorpb.DescriptorProto {
	t.Helper()
	for _, candidate := range protos["orka/storage/v1/"+file].GetMessageType() {
		if candidate.GetName() == name {
			return candidate
		}
	}
	t.Fatalf("no message %s in %s", name, file)
	return nil
}

// A persisted field tagged json:"-" is an ordinary wire field: leaving it
// out of the wire fails generation.
func TestGenerationFailsWhenAHiddenGoFieldHasNoWireField(t *testing.T) {
	contract := editedContract(t, "\tGoalComplete bool ", "\tReviewerNote string `json:\"-\"`\n\tGoalComplete bool ")
	expectProblem(t, contract, wireFiles(), `PlanState.ReviewerNote (json "-") has no field in wire message PlanState`)
}

func TestGenerationFailsWhenAWireFieldHasNoGoField(t *testing.T) {
	files := editedWire(t, func(protos map[string]*descriptorpb.FileDescriptorProto) {
		plan := message(t, protos, "results.proto", "PlanState")
		plan.Field = append(plan.Field, &descriptorpb.FieldDescriptorProto{
			Name: new("archived"), Number: new(int32(20)), JsonName: new("archived"),
			Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_BOOL.Enum(),
		})
	})
	expectProblem(t, testContractDir, files, "wire field orka.storage.v1.PlanState.archived has no field in Go record PlanState")
}

func TestGenerationFailsWhenAMethodHasNoRPC(t *testing.T) {
	files := editedWire(t, func(protos map[string]*descriptorpb.FileDescriptorProto) {
		results := protos["orka/storage/v1/results.proto"].GetService()[0]
		var kept []*descriptorpb.MethodDescriptorProto
		for _, method := range results.GetMethod() {
			if method.GetName() != "DeletePlan" {
				kept = append(kept, method)
			}
		}
		results.Method = kept
	})
	expectProblem(t, testContractDir, files, "method DeletePlan has no RPC in service Results")
}

func TestGenerationFailsWhenAnRPCHasNoMethod(t *testing.T) {
	files := editedWire(t, func(protos map[string]*descriptorpb.FileDescriptorProto) {
		results := protos["orka/storage/v1/results.proto"].GetService()[0]
		extra := proto.Clone(results.GetMethod()[0]).(*descriptorpb.MethodDescriptorProto)
		extra.Name = new("ArchivePlan")
		results.Method = append(results.Method, extra)
	})
	expectProblem(t, testContractDir, files, "RPC orka.storage.v1.Results.ArchivePlan has no method")
}

// Parameters map by name, so renaming one in Go fails generation until the
// wire changes; swapping two string parameters cannot go unnoticed.
func TestGenerationFailsWhenAParameterIsRenamed(t *testing.T) {
	contract := editedContract(t, "DeletePlan(ctx context.Context, namespace, taskName string) error",
		"DeletePlan(ctx context.Context, namespace, task string) error")
	expectProblem(t, contract, wireFiles(),
		"parameter task of DeletePlan has no field in DeletePlanRequest",
		"request field orka.storage.v1.DeletePlanRequest.task_name has no parameter in method DeletePlan")
}

func TestGenerationFailsOnATypeOutsideTheMappingTable(t *testing.T) {
	files := editedWire(t, func(protos map[string]*descriptorpb.FileDescriptorProto) {
		for _, field := range message(t, protos, "results.proto", "PlanState").GetField() {
			if field.GetName() == "iteration" {
				field.Type = descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()
			}
		}
	})
	expectProblem(t, testContractDir, files, "the Go type int and wire field orka.storage.v1.PlanState.iteration (string) are not a pair")
}

func TestGenerationFailsOnInconsistentRevisions(t *testing.T) {
	files := editedWire(t, func(protos map[string]*descriptorpb.FileDescriptorProto) {
		for _, method := range protos["orka/storage/v1/results.proto"].GetService()[0].GetMethod() {
			if method.GetName() == "GetPlan" {
				proto.SetExtension(method.GetOptions(), storagepb.E_RpcSince, int32(2))
			}
		}
		for _, field := range message(t, protos, "results.proto", "SavePlanRequest").GetField() {
			if field.GetName() == "plan" {
				if field.Options == nil {
					field.Options = &descriptorpb.FieldOptions{}
				}
				proto.SetExtension(field.Options, storagepb.E_Since, int32(1))
			}
		}
		for _, method := range protos["orka/storage/v1/messages.proto"].GetService()[0].GetMethod() {
			if method.GetName() == "SendMessage" {
				proto.SetExtension(method.GetOptions(), storagepb.E_Kind, storagepb.MethodKind_METHOD_KIND_UNSPECIFIED)
			}
		}
	})
	expectProblem(t, testContractDir, files,
		"orka.storage.v1.Results.GetPlan has rpc_since 2 outside 1 to 1",
		"request field orka.storage.v1.SavePlanRequest.plan has since 1",
		"orka.storage.v1.Messages.SendMessage declares no kind")
}

func TestGenerationFailsWhenAMessageHasNoGoRecord(t *testing.T) {
	files := editedWire(t, func(protos map[string]*descriptorpb.FileDescriptorProto) {
		file := protos["orka/storage/v1/results.proto"]
		file.MessageType = append(file.MessageType, &descriptorpb.DescriptorProto{Name: new("PlanHistory")})
	})
	expectProblem(t, testContractDir, files, "wire message orka.storage.v1.PlanHistory has no Go contract record")
}

func TestSnakeCaseMatchesWireNames(t *testing.T) {
	for goName, want := range map[string]string{
		"DatasetID": "dataset_id", "taskName": "task_name", "ID": "id", "HTTPStatus": "http_status",
		"TaskUID": "task_uid", "MaxArtifactBytes": "max_artifact_bytes", "msg": "msg", "Retry2After": "retry2_after",
	} {
		if got := snakeCase(goName); got != want {
			t.Errorf("snakeCase(%q) = %q, want %q", goName, got, want)
		}
	}
}

// goCamelCase must name exactly the fields protoc-gen-go generated, or the
// generated mapping would not compile.
func TestGoCamelCaseMatchesProtocGenGo(t *testing.T) {
	checked := 0
	for _, file := range wireFiles() {
		for index := 0; index < file.Messages().Len(); index++ {
			desc := file.Messages().Get(index)
			messageType, err := protoregistry.GlobalTypes.FindMessageByName(desc.FullName())
			if err != nil {
				t.Fatal(err)
			}
			goType := reflect.TypeOf(messageType.Zero().Interface()).Elem()
			if goType.Name() != goCamelCase(string(desc.Name())) {
				t.Errorf("message %s is generated as %s", desc.Name(), goType.Name())
			}
			for fieldIndex := 0; fieldIndex < desc.Fields().Len(); fieldIndex++ {
				name := goCamelCase(string(desc.Fields().Get(fieldIndex).Name()))
				if _, ok := goType.FieldByName(name); !ok {
					t.Errorf("%s has no Go field %s", goType.Name(), name)
				}
				checked++
			}
		}
	}
	if checked == 0 {
		t.Fatal("no wire fields checked")
	}
}

func TestModelCoversTheMappedFamilies(t *testing.T) {
	m := realModel(t)
	methods := map[string]bool{}
	for _, svc := range m.services {
		for _, method := range svc.methods {
			methods[method.name] = true
		}
	}
	for _, name := range []string{"Describe", "HealthCheck", "SendMessage", "GetMessages", "DeleteTaskMessages",
		"DeleteParentMessages", "SavePlan", "GetPlan", "DeletePlan", "SaveResult", "GetResult", "DeleteResult",
		"SavePromptResultReceipt", "GetPromptResultReceipt", "SaveArtifact", "GetArtifact", "ListArtifacts", "DeleteArtifacts"} {
		if !methods[name] {
			t.Errorf("method %s has no RPC", name)
		}
	}
	records := map[string]bool{}
	for _, rec := range m.records {
		records[rec.name] = true
	}
	for _, name := range []string{"Message", "PlanState", "ProviderInfo", "ProviderLimits", "WriteEnvelope", "ControllerEpochFence",
		"PromptResultReceipt", "ArtifactOwner", "ArtifactKey", "ArtifactMetadata", "SaveArtifactRequest"} {
		if !records[name] {
			t.Errorf("record %s is not mapped", name)
		}
	}
}

func methodProto(t *testing.T, protos map[string]*descriptorpb.FileDescriptorProto, file, name string) *descriptorpb.MethodDescriptorProto {
	t.Helper()
	for _, method := range protos["orka/storage/v1/"+file].GetService()[0].GetMethod() {
		if method.GetName() == name {
			return method
		}
	}
	t.Fatalf("no RPC %s in %s", name, file)
	return nil
}

func field(t *testing.T, message *descriptorpb.DescriptorProto, name string) *descriptorpb.FieldDescriptorProto {
	t.Helper()
	for _, candidate := range message.GetField() {
		if candidate.GetName() == name {
			return candidate
		}
	}
	t.Fatalf("no field %s in %s", name, message.GetName())
	return nil
}

func setBlob(field *descriptorpb.FieldDescriptorProto, blob bool) {
	if field.Options == nil {
		field.Options = &descriptorpb.FieldOptions{}
	}
	proto.SetExtension(field.Options, storagepb.E_Blob, blob)
}

// A streaming RPC streams exactly one blob in its direction, and the stream
// direction matches its kind.
func TestGenerationFailsOnInconsistentStreams(t *testing.T) {
	files := editedWire(t, func(protos map[string]*descriptorpb.FileDescriptorProto) {
		// A unary RPC with a blob.
		setBlob(field(t, message(t, protos, "messages.proto", "Message"), "content"), true)
		// A streaming read without its blob.
		setBlob(field(t, message(t, protos, "results.proto", "GetResultResponse"), "data"), false)
		// A streaming write declared as a read.
		proto.SetExtension(methodProto(t, protos, "artifacts.proto", "SaveArtifact").GetOptions(), storagepb.E_Kind,
			storagepb.MethodKind_METHOD_KIND_READ)
		// A stream in both directions.
		methodProto(t, protos, "results.proto", "GetPromptResultReceipt").ClientStreaming = new(true)
	})
	expectProblem(t, testContractDir, files,
		"blob field orka.storage.v1.Message.content must be a singular bytes field",
		"orka.storage.v1.Results.GetResult has 0 blob fields in its request and 0 in its response",
		"orka.storage.v1.Artifacts.SaveArtifact streams its request, so it must be a write",
		"orka.storage.v1.Results.GetPromptResultReceipt streams in both directions")
}

// A blob field must be the blob of a streaming RPC.
func TestGenerationFailsOnABlobOutsideAStream(t *testing.T) {
	files := editedWire(t, func(protos map[string]*descriptorpb.FileDescriptorProto) {
		setBlob(field(t, message(t, protos, "common.proto", "WriteEnvelope"), "request_digest"), true)
	})
	expectProblem(t, testContractDir, files,
		"blob field orka.storage.v1.WriteEnvelope.request_digest is not the blob of a streaming RPC")
}

// SaveArtifact takes the record named SaveArtifactRequest, whose message is
// the RPC's request; renaming the record breaks the pairing.
func TestGenerationMapsAnInlineRecordToTheRequest(t *testing.T) {
	m := realModel(t)
	var inline *rpcMethod
	for _, svc := range m.services {
		for _, method := range svc.methods {
			if method.name == "SaveArtifact" {
				inline = method
			}
		}
	}
	if inline == nil || inline.inline == nil || inline.inline.name != "SaveArtifactRequest" || inline.stream != streamUpload {
		t.Fatalf("SaveArtifact does not map its request record inline: %+v", inline)
	}
	contract := editedContract(t, "SaveArtifact(ctx context.Context, request SaveArtifactRequest) error",
		"SaveArtifact(ctx context.Context, request SaveArtifactRequest, overwrite bool) error")
	expectProblem(t, contract, wireFiles(), "parameter request of SaveArtifact has no field in SaveArtifactRequest")
}

package wire

import (
	"runtime/debug"
	"strings"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/wire/storagepb"
)

// ProtocolRevision is this library's orka.storage.v1 protocol revision. A
// release raises it whenever it adds RPCs or request fields.
const ProtocolRevision = 1

// Call metadata keys. The contract and revision headers travel on every
// request and response. The dataset header names the dataset a request
// expects and the dataset a response came from; every call but those of the
// Dataset service, which a client makes to learn its dataset, must name one.
const (
	HeaderContract = "orka-storage-contract"
	HeaderRevision = "orka-storage-revision"
	HeaderDataset  = "orka-storage-dataset"
	// HeaderEnvelope carries a write's binary WriteEnvelope message. Only a
	// write carries one.
	HeaderEnvelope = "orka-storage-envelope-bin"
	// HeaderTaskDataFence carries the binary TaskDataFence message of a call
	// made under a Task data fence (see storagev1.WithTaskDataFence).
	HeaderTaskDataFence = "orka-storage-task-data-fence-bin"
)

// MaxMessageBytes caps a unary request or response message in each
// direction.
const MaxMessageBytes = 16 << 20

// method describes one RPC. The generated method table holds one per
// contract method that has a wire RPC, with the kind the contract's method
// catalog gives it.
type method struct {
	name     string
	fullName string
	kind     storagev1.MethodKind
	since    int
}

// mayChange reports whether the method may change stored state, so that a
// transport failure after its request may have been sent is an unknown
// outcome. Writes and administrative calls may; reads never do.
func (m *method) mayChange() bool {
	return m.kind != storagev1.MethodRead
}

// datasetServicePrefix starts the full name of every Dataset service RPC.
var datasetServicePrefix = "/" + storagepb.Dataset_ServiceDesc.ServiceName + "/"

// mayOmitDataset reports whether a call may leave out the dataset header:
// only Describe and HealthCheck may, so a client can learn its dataset.
func (m *method) mayOmitDataset() bool {
	return strings.HasPrefix(m.fullName, datasetServicePrefix)
}

func methodByFullName(fullName string) *method {
	return methodsByFullName[fullName]
}

// ServerInfo describes the server library that serves a provider. Describe
// returns it beside the provider's own description.
type ServerInfo struct {
	// ProtocolRevision is the server library's protocol revision.
	ProtocolRevision int64
	// LibraryVersion is the version of the server library's Go module.
	LibraryVersion string
	// AdapterName and AdapterVersion identify the adapter binary.
	AdapterName    string
	AdapterVersion string
}

const maxServerInfoText = 128

func serverInfoToWire(info ServerInfo) *storagepb.ServerInfo {
	return &storagepb.ServerInfo{
		ProtocolRevision: info.ProtocolRevision,
		LibraryVersion:   sanitizeText(info.LibraryVersion, maxServerInfoText),
		AdapterName:      sanitizeText(info.AdapterName, maxServerInfoText),
		AdapterVersion:   sanitizeText(info.AdapterVersion, maxServerInfoText),
	}
}

func serverInfoFromWire(info *storagepb.ServerInfo) ServerInfo {
	return ServerInfo{
		ProtocolRevision: info.GetProtocolRevision(),
		LibraryVersion:   sanitizeText(info.GetLibraryVersion(), maxServerInfoText),
		AdapterName:      sanitizeText(info.GetAdapterName(), maxServerInfoText),
		AdapterVersion:   sanitizeText(info.GetAdapterVersion(), maxServerInfoText),
	}
}

const modulePath = "github.com/orka-agents/orka/pkg/storage"

// libraryVersion reports the storage module version linked into this binary.
func libraryVersion() string {
	build, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	if build.Main.Path == modulePath {
		return build.Main.Version
	}
	for _, dependency := range build.Deps {
		if dependency.Path == modulePath {
			if dependency.Replace != nil && dependency.Replace.Version != "" {
				return dependency.Replace.Version
			}
			return dependency.Version
		}
	}
	return "unknown"
}

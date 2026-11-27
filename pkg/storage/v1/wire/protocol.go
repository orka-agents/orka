package wire

import (
	"runtime/debug"
	"strings"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/wire/storagepb"
)

const ProtocolRevision = 4

const (
	HeaderContract      = "orka-storage-contract"
	HeaderRevision      = "orka-storage-revision"
	HeaderDataset       = "orka-storage-dataset-bin"
	HeaderEnvelope      = "orka-storage-envelope-bin"
	HeaderTaskDataFence = "orka-storage-task-data-fence-bin"
	HeaderBlobSize      = "orka-storage-blob-size"
	HeaderBlobSHA256    = "orka-storage-blob-sha256"
	HeaderListCount     = "orka-storage-list-count"
)

const unknownName = "unknown"

const MaxMessageBytes = 16 << 20

const MaxChunkBytes = 1 << 20

const DefaultMaxBlobBytes = 64 << 20

type streamShape int

const (
	streamNone streamShape = iota
	streamUpload
	streamDownload
	streamList
)

type method struct {
	name     string
	fullName string
	kind     storagev1.MethodKind
	class    storagev1.CallClass
	since    int
	stream   streamShape
	contract string
}

func (m *method) mayChange() bool {
	return m.kind != storagev1.MethodRead
}

var datasetServicePrefix = "/" + storagepb.Dataset_ServiceDesc.ServiceName + "/"

func (m *method) mayOmitDataset() bool {
	return strings.HasPrefix(m.fullName, datasetServicePrefix)
}

func methodByFullName(fullName string) *method {
	return methodsByFullName[fullName]
}

type ServerInfo struct {
	ProtocolRevision int64
	LibraryVersion   string
	AdapterName      string
	AdapterVersion   string
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

func libraryVersion() string {
	build, ok := debug.ReadBuildInfo()
	if !ok {
		return unknownName
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
	return unknownName
}

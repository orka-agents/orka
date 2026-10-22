package wire

import (
	"runtime/debug"

	"github.com/orka-agents/orka/pkg/storage/v1/wire/storagepb"
)

// ProtocolRevision is this library's orka.storage.v1 protocol revision. A
// release raises it whenever it adds RPCs or request fields.
const ProtocolRevision = 1

// Call metadata keys. The contract and revision headers travel on every
// request and response; the dataset header names the dataset a request
// expects and the dataset a response came from.
const (
	HeaderContract = "orka-storage-contract"
	HeaderRevision = "orka-storage-revision"
	HeaderDataset  = "orka-storage-dataset"
	// HeaderEnvelope carries a write's binary WriteEnvelope message.
	HeaderEnvelope = "orka-storage-envelope-bin"
	// HeaderBlobSize and HeaderBlobSHA256 describe the whole blob a streaming
	// RPC carries: its size in decimal bytes and its SHA-256 digest in
	// lowercase hex. An upload sends them as request headers and a download
	// as response headers.
	HeaderBlobSize   = "orka-storage-blob-size"
	HeaderBlobSHA256 = "orka-storage-blob-sha256"
)

// unknownName names a kind, class or version this library cannot name.
const unknownName = "unknown"

// MaxMessageBytes caps a unary request or response message in each
// direction.
const MaxMessageBytes = 16 << 20

// MaxChunkBytes caps the blob bytes one message of a streaming RPC carries,
// so a large transfer moves in small messages that share flow control
// fairly.
const MaxChunkBytes = 1 << 20

// DefaultMaxBlobBytes is the largest blob a client or server accepts unless
// it is configured otherwise. It covers the contract's largest values.
const DefaultMaxBlobBytes = 64 << 20

// Kind states whether a method may change stored state.
type Kind int

const (
	// KindRead never changes stored state, so a failed call is safe to
	// repeat.
	KindRead Kind = iota + 1
	// KindWrite may change stored state. A transport failure after the
	// request may have been sent is an unknown outcome.
	KindWrite
)

// String returns the kind's name.
func (k Kind) String() string {
	switch k {
	case KindRead:
		return "read"
	case KindWrite:
		return "write"
	default:
		return unknownName
	}
}

// Class groups calls that share a concurrency limit and deadline, so a slow
// class cannot starve another.
type Class int

const (
	// ClassHealth is Describe and HealthCheck.
	ClassHealth Class = iota + 1
	// ClassControl is every other unary call.
	ClassControl
	// ClassBulk is every streaming call, which carries a blob. A client sends
	// bulk calls over their own connection when it has one.
	ClassBulk
)

// String returns the class's name.
func (c Class) String() string {
	switch c {
	case ClassHealth:
		return "health"
	case ClassControl:
		return "control"
	case ClassBulk:
		return "bulk"
	default:
		return unknownName
	}
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
)

// method describes one RPC. The generated method table holds one per
// contract method that has a wire RPC.
type method struct {
	name     string
	fullName string
	kind     Kind
	since    int
	stream   streamShape
	// contract is the contract part the RPC belongs to, or empty for the
	// Dataset RPCs every provider serves.
	contract string
}

func methodByFullName(fullName string) *method {
	return methodsByFullName[fullName]
}

// class returns the call class of m.
func (m *method) class() Class {
	switch {
	case m.contract == "":
		return ClassHealth
	case m.stream != streamNone:
		return ClassBulk
	default:
		return ClassControl
	}
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

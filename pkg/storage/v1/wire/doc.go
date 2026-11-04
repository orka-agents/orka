// Package wire carries the orka.storage.v1 contract over gRPC.
//
// The Go contract in github.com/orka-agents/orka/pkg/storage/v1 stays the
// source of semantics. The .proto files under pkg/storage/proto are the wire
// source of truth, and wiregen generates the mapping between the two: one RPC
// per contract method, one wire message per contract record. Generation fails
// when any Go parameter, result or record field, including fields tagged
// json:"-", has no wire counterpart, so every persisted field crosses the
// wire.
//
// A Client implements the contract over a gRPC connection and returns the
// contract's typed errors. A Server serves a provider and encodes its errors
// with a google.rpc.ErrorInfo reason. Transport failures are classified with
// per-call send evidence: a call whose request never left the process returns
// [storagev1.ErrUnavailable], and a write that may have been sent returns
// [storagev1.ErrOutcomeUnknown].
//
// Every call names the dataset it expects, and the Server refuses a call for
// another dataset before the provider runs. Blobs, such as artifact bytes,
// move on streaming RPCs in parts of at most [MaxChunkBytes], and the Server
// calls the provider only with a whole blob that matches its declared size
// and digest. Package adapterkit serves a provider as a separately deployed
// adapter with TLS, client authentication, limits and probes.
package wire

//go:generate go run ./internal/wiregen

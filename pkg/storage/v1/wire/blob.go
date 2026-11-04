package wire

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/orka-agents/orka/pkg/storage/v1/wire/storagepb"
)

// Blob framing. A streaming RPC carries one blob field in the direction it
// streams. The first message carries every other field and at most
// MaxChunkBytes of the blob; each later message carries only the next part of
// the blob, never empty, in the same field. The blob's total size and SHA-256
// digest travel as headers (HeaderBlobSize, HeaderBlobSHA256), so the
// receiver can refuse an oversized blob before reading it and checks the
// reassembled bytes before using them.

var (
	// errBlobFraming marks a stream that breaks the framing rules.
	errBlobFraming = errors.New("the blob stream breaks the framing rules")
	// errBlobShort marks a stream that ended before the declared size.
	errBlobShort = errors.New("the blob stream ended before the declared size")
	// errBlobDigest marks a reassembled blob whose digest does not match.
	errBlobDigest = errors.New("the blob does not match its declared digest")
)

// blobPaths caches, per message type, the fields from the message down to
// its blob field.
var blobPaths sync.Map

// blobPathOf returns the fields from a framed message down to its blob
// field. The generator guarantees exactly one such path for every message a
// streaming RPC frames.
func blobPathOf(desc protoreflect.MessageDescriptor) []protoreflect.FieldDescriptor {
	if cached, ok := blobPaths.Load(desc.FullName()); ok {
		return cached.([]protoreflect.FieldDescriptor)
	}
	path := findBlob(desc, nil, map[protoreflect.FullName]bool{})
	blobPaths.Store(desc.FullName(), path)
	return path
}

func findBlob(desc protoreflect.MessageDescriptor, prefix []protoreflect.FieldDescriptor,
	visiting map[protoreflect.FullName]bool) []protoreflect.FieldDescriptor {
	if visiting[desc.FullName()] {
		return nil
	}
	visiting[desc.FullName()] = true
	defer delete(visiting, desc.FullName())
	for index := 0; index < desc.Fields().Len(); index++ {
		field := desc.Fields().Get(index)
		path := append(append([]protoreflect.FieldDescriptor(nil), prefix...), field)
		if isBlobField(field) {
			return path
		}
		if field.Message() != nil && !field.IsList() && !field.IsMap() {
			if found := findBlob(field.Message(), path, visiting); found != nil {
				return found
			}
		}
	}
	return nil
}

func isBlobField(field protoreflect.FieldDescriptor) bool {
	value, _ := proto.GetExtension(field.Options(), storagepb.E_Blob).(bool)
	return value
}

// takeBlob removes the blob from message and returns it. An absent parent
// message means an empty blob.
func takeBlob(message protoreflect.Message, path []protoreflect.FieldDescriptor) []byte {
	for _, field := range path[:len(path)-1] {
		if !message.Has(field) {
			return nil
		}
		message = message.Mutable(field).Message()
	}
	last := path[len(path)-1]
	data := message.Get(last).Bytes()
	message.Clear(last)
	return data
}

// setBlob stores data in message's blob field, creating its parent messages.
func setBlob(message protoreflect.Message, path []protoreflect.FieldDescriptor, data []byte) {
	if len(data) == 0 {
		return
	}
	for _, field := range path[:len(path)-1] {
		message = message.Mutable(field).Message()
	}
	message.Set(path[len(path)-1], protoreflect.ValueOfBytes(data))
}

// restoreBlob stores a reassembled blob in the first message. A non-empty
// blob needs the parent messages the first message carried; it never
// creates a record the sender left out.
func restoreBlob(message protoreflect.Message, path []protoreflect.FieldDescriptor, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	for _, field := range path[:len(path)-1] {
		if !message.Has(field) {
			return fmt.Errorf("%w: the blob belongs to %s, which the first message leaves out", errBlobFraming, field.FullName())
		}
		message = message.Mutable(field).Message()
	}
	message.Set(path[len(path)-1], protoreflect.ValueOfBytes(data))
	return nil
}

// blobPart returns the blob part a later message carries, and refuses a
// later message that carries any other field.
func blobPart(message protoreflect.Message, path []protoreflect.FieldDescriptor) ([]byte, error) {
	for index, field := range path {
		other := false
		message.Range(func(populated protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
			other = populated.Number() != field.Number()
			return !other
		})
		if other {
			return nil, fmt.Errorf("%w: a later message carries a field other than the blob", errBlobFraming)
		}
		if !message.Has(field) {
			return nil, fmt.Errorf("%w: a later message carries no part of the blob", errBlobFraming)
		}
		if index == len(path)-1 {
			return message.Get(field).Bytes(), nil
		}
		message = message.Get(field).Message()
	}
	return nil, nil
}

// sendBlob sends message with data as its blob, in parts of at most
// MaxChunkBytes. message must not carry its blob already.
func sendBlob(send func(any) error, message proto.Message, path []protoreflect.FieldDescriptor, data []byte) error {
	first := data[:min(len(data), MaxChunkBytes)]
	setBlob(message.ProtoReflect(), path, first)
	if err := send(message); err != nil {
		return err
	}
	for rest := data[len(first):]; len(rest) > 0; {
		size := min(len(rest), MaxChunkBytes)
		part := message.ProtoReflect().New()
		setBlob(part, path, rest[:size])
		if err := send(part.Interface()); err != nil {
			return err
		}
		rest = rest[size:]
	}
	return nil
}

// blobHeader is the size and digest a framed stream declares.
type blobHeader struct {
	size   int64
	digest []byte
}

func (h blobHeader) pairs() []string {
	return []string{HeaderBlobSize, strconv.FormatInt(h.size, 10), HeaderBlobSHA256, hex.EncodeToString(h.digest)}
}

func describeBlob(data []byte) blobHeader {
	sum := sha256.Sum256(data)
	return blobHeader{size: int64(len(data)), digest: sum[:]}
}

// parseBlobHeader reads the size and digest headers of a framed stream.
func parseBlobHeader(header metadata.MD) (blobHeader, error) {
	sizes, digests := header.Get(HeaderBlobSize), header.Get(HeaderBlobSHA256)
	if len(sizes) != 1 || len(digests) != 1 {
		return blobHeader{}, fmt.Errorf("%w: the stream must declare its blob size and digest once", errBlobFraming)
	}
	size, err := strconv.ParseInt(sizes[0], 10, 64)
	if err != nil || size < 0 {
		return blobHeader{}, fmt.Errorf("%w: the declared blob size is invalid", errBlobFraming)
	}
	digest, err := hex.DecodeString(digests[0])
	if err != nil || len(digest) != sha256.Size {
		return blobHeader{}, fmt.Errorf("%w: the declared blob digest is invalid", errBlobFraming)
	}
	return blobHeader{size: size, digest: digest}, nil
}

// reassemble reads the rest of a framed stream after its first message and
// stores the whole blob in first. next returns io.EOF when the stream ends.
// It never reads more than the declared size, which the caller has already
// checked against its limit.
func reassemble(first proto.Message, next func() (proto.Message, error), declared blobHeader) error {
	path := blobPathOf(first.ProtoReflect().Descriptor())
	part := takeBlob(first.ProtoReflect(), path)
	if len(part) > MaxChunkBytes {
		return fmt.Errorf("%w: a message carries more than %d bytes of the blob", errBlobFraming, MaxChunkBytes)
	}
	if int64(len(part)) > declared.size {
		return fmt.Errorf("%w: the stream carries more than its declared size", errBlobFraming)
	}
	data := make([]byte, 0, declared.size)
	data = append(data, part...)
	for {
		message, err := next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		part, err := blobPart(message.ProtoReflect(), path)
		if err != nil {
			return err
		}
		if len(part) > MaxChunkBytes {
			return fmt.Errorf("%w: a message carries more than %d bytes of the blob", errBlobFraming, MaxChunkBytes)
		}
		if int64(len(data))+int64(len(part)) > declared.size {
			return fmt.Errorf("%w: the stream carries more than its declared size", errBlobFraming)
		}
		data = append(data, part...)
	}
	if int64(len(data)) != declared.size {
		return errBlobShort
	}
	if sum := sha256.Sum256(data); !bytes.Equal(sum[:], declared.digest) {
		return errBlobDigest
	}
	return restoreBlob(first.ProtoReflect(), path, data)
}

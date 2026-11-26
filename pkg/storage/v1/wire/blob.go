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

var (
	errBlobFraming = errors.New("the blob stream breaks the framing rules")
	errBlobShort   = errors.New("the blob stream ended before the declared size")
	errBlobDigest  = errors.New("the blob does not match its declared digest")
)

var blobPaths sync.Map

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

func setBlob(message protoreflect.Message, path []protoreflect.FieldDescriptor, data []byte) {
	if len(data) == 0 {
		return
	}
	for _, field := range path[:len(path)-1] {
		message = message.Mutable(field).Message()
	}
	message.Set(path[len(path)-1], protoreflect.ValueOfBytes(data))
}

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

func reassemble(first proto.Message, next func() (proto.Message, error), declared blobHeader) error {
	path := blobPathOf(first.ProtoReflect().Descriptor())
	part := takeBlob(first.ProtoReflect(), path)
	if len(part) > MaxChunkBytes {
		return fmt.Errorf("%w: a message carries more than %d bytes of the blob", errBlobFraming, MaxChunkBytes)
	}
	if int64(len(part)) > declared.size {
		return fmt.Errorf("%w: the stream carries more than its declared size", errBlobFraming)
	}
	data := make([]byte, 0, min(declared.size, int64(4*MaxChunkBytes)))
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

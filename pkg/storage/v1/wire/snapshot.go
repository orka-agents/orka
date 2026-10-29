package wire

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Batched reads. A read whose result the contract does not bound by size,
// such as a Session with its whole transcript, returns its list of records in
// batches on a server-streaming RPC. The provider still produces the whole
// result at once, from one snapshot; the server library splits the list only
// after the provider returned, so the stream holds nothing open in the
// provider. The first message carries every other response field and the
// first batch; each later message carries only the next batch, never empty,
// in the same list field. A batch holds records of at most MaxChunkBytes in
// total, or a single larger record, and every message stays within
// MaxMessageBytes. The response headers HeaderSnapshotRecords and
// HeaderSnapshotBytes declare the whole list: its number of records and their
// encoded size in the list, each record's field tag and length prefix
// included, so even an empty record counts. The client refuses a list with
// more records or bytes than it accepts before reading any of it, and checks
// the reassembled list against the declaration.

// errSnapshotFraming marks a batched response that breaks the framing rules
// or does not match its declared size.
var errSnapshotFraming = errors.New("the batched response breaks the framing rules")

// batchedPaths caches, per response type and path, the fields from the
// response message down to its batched list.
var batchedPaths sync.Map

// batchedPathOf returns the fields from a batched read's response message
// down to the list it batches. The generator guarantees that path names
// singular message fields ending in a repeated message field.
func batchedPathOf(desc protoreflect.MessageDescriptor, path string) []protoreflect.FieldDescriptor {
	key := string(desc.FullName()) + "|" + path
	if cached, ok := batchedPaths.Load(key); ok {
		return cached.([]protoreflect.FieldDescriptor)
	}
	names := strings.Split(path, ".")
	fields := make([]protoreflect.FieldDescriptor, 0, len(names))
	message := desc
	for _, name := range names {
		field := message.Fields().ByName(protoreflect.Name(name))
		fields = append(fields, field)
		message = field.Message()
	}
	batchedPaths.Store(key, fields)
	return fields
}

// takeRecords removes the batched list from message and returns its records.
// An absent parent message means an empty list.
func takeRecords(message protoreflect.Message, path []protoreflect.FieldDescriptor) []protoreflect.Message {
	for _, field := range path[:len(path)-1] {
		if !message.Has(field) {
			return nil
		}
		message = message.Mutable(field).Message()
	}
	last := path[len(path)-1]
	if !message.Has(last) {
		return nil
	}
	list := message.Get(last).List()
	records := make([]protoreflect.Message, list.Len())
	for index := range records {
		records[index] = list.Get(index).Message()
	}
	message.Clear(last)
	return records
}

// putRecords appends records to message's batched list. create allows it to
// create the list's parent messages; a reassembled list never creates a
// record the first message left out.
func putRecords(message protoreflect.Message, path []protoreflect.FieldDescriptor, records []protoreflect.Message, create bool) error {
	if len(records) == 0 {
		return nil
	}
	for _, field := range path[:len(path)-1] {
		if !create && !message.Has(field) {
			return fmt.Errorf("%w: the records belong to %s, which the first message leaves out", errSnapshotFraming, field.FullName())
		}
		message = message.Mutable(field).Message()
	}
	list := message.Mutable(path[len(path)-1]).List()
	for _, record := range records {
		list.Append(protoreflect.ValueOfMessage(record))
	}
	return nil
}

// laterRecords returns the batch a later message carries, and refuses a later
// message that carries any other field or no records.
func laterRecords(message protoreflect.Message, path []protoreflect.FieldDescriptor) ([]protoreflect.Message, error) {
	for index, field := range path {
		other := false
		message.Range(func(populated protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
			other = populated.Number() != field.Number()
			return !other
		})
		if other {
			return nil, fmt.Errorf("%w: a later message carries a field other than the batched list", errSnapshotFraming)
		}
		if !message.Has(field) {
			return nil, fmt.Errorf("%w: a later message carries no records", errSnapshotFraming)
		}
		if index < len(path)-1 {
			message = message.Get(field).Message()
			continue
		}
		list := message.Get(field).List()
		records := make([]protoreflect.Message, list.Len())
		for position := range records {
			records[position] = list.Get(position).Message()
		}
		return records, nil
	}
	return nil, nil
}

// recordSize is the encoded size of record as an element of the list field:
// its tag, its length prefix and its bytes. It is at least two bytes, so a
// byte limit also bounds the number of records.
func recordSize(field protoreflect.FieldDescriptor, record protoreflect.Message) int64 {
	return int64(protowire.SizeTag(field.Number()) + protowire.SizeBytes(proto.Size(record.Interface())))
}

// snapshotHeader is the list a batched response declares.
type snapshotHeader struct {
	records int64
	bytes   int64
}

func (h snapshotHeader) pairs() []string {
	return []string{HeaderSnapshotRecords, strconv.FormatInt(h.records, 10), HeaderSnapshotBytes, strconv.FormatInt(h.bytes, 10)}
}

// parseSnapshotHeader reads the record count and size headers of a batched
// response.
func parseSnapshotHeader(header metadata.MD) (snapshotHeader, error) {
	records, sizes := header.Get(HeaderSnapshotRecords), header.Get(HeaderSnapshotBytes)
	if len(records) != 1 || len(sizes) != 1 {
		return snapshotHeader{}, fmt.Errorf("%w: the response must declare its record count and size once", errSnapshotFraming)
	}
	count, countErr := strconv.ParseInt(records[0], 10, 64)
	size, sizeErr := strconv.ParseInt(sizes[0], 10, 64)
	if countErr != nil || sizeErr != nil || count < 0 || size < 0 {
		return snapshotHeader{}, fmt.Errorf("%w: the declared record count or size is invalid", errSnapshotFraming)
	}
	return snapshotHeader{records: count, bytes: size}, nil
}

// batchRecords splits records, the elements of list field field, into
// batches of at most MaxChunkBytes in total, with at least one record in each
// batch, and returns the batches and the records' total size (recordSize).
func batchRecords(field protoreflect.FieldDescriptor, records []protoreflect.Message) ([][]protoreflect.Message, int64) {
	var batches [][]protoreflect.Message
	var total, size int64
	start := 0
	for index, record := range records {
		encoded := recordSize(field, record)
		total += encoded
		if index > start && size+encoded > MaxChunkBytes {
			batches = append(batches, records[start:index])
			start, size = index, 0
		}
		size += encoded
	}
	if start < len(records) {
		batches = append(batches, records[start:])
	}
	return batches, total
}

// sendSnapshot sends a batched read's response with its list in batches. The
// whole response is checked and split before anything is sent, so a list
// the server cannot send fails as a provider error and no part of it reaches
// the client.
func (s *Server) sendSnapshot(stream grpc.ServerStream, m *method, response proto.Message) error {
	ctx := stream.Context()
	if field, invalid := firstInvalidString(response.ProtoReflect()); invalid {
		return s.encodeError(ctx, m, fmt.Errorf("response field %s is not valid UTF-8", field))
	}
	path := batchedPathOf(response.ProtoReflect().Descriptor(), m.batched)
	records := takeRecords(response.ProtoReflect(), path)
	batches, total := batchRecords(path[len(path)-1], records)
	messages := []proto.Message{response}
	for index, batch := range batches {
		message := response
		if index > 0 {
			message = response.ProtoReflect().New().Interface()
			messages = append(messages, message)
		}
		if err := putRecords(message.ProtoReflect(), path, batch, true); err != nil {
			return s.encodeError(ctx, m, err)
		}
	}
	for _, message := range messages {
		if size := proto.Size(message); size > MaxMessageBytes {
			return s.encodeError(ctx, m, fmt.Errorf("a message of the batched response is %d bytes, over the %d-byte limit",
				size, MaxMessageBytes))
		}
	}
	declared := snapshotHeader{records: int64(len(records)), bytes: total}
	if err := stream.SetHeader(metadata.Pairs(declared.pairs()...)); err != nil {
		return status.Error(codes.Internal, "cannot set storage response headers")
	}
	// stream is the call's checkedStream, which bounds each send by the
	// call's duration limit.
	for _, message := range messages {
		if err := stream.SendMsg(message); err != nil {
			return err
		}
	}
	return nil
}

// reassembleRecords reads the rest of a batched response after its first
// message and appends every later batch to first's list. next returns io.EOF
// when the stream ends. It never accepts more records or bytes than declared,
// which the caller has already checked against its limit.
func reassembleRecords(first proto.Message, path []protoreflect.FieldDescriptor, declared snapshotHeader,
	next func() (proto.Message, error)) error {
	var count, size int64
	list := path[len(path)-1]
	account := func(records []protoreflect.Message) error {
		for _, record := range records {
			count++
			size += recordSize(list, record)
		}
		if count > declared.records || size > declared.bytes {
			return fmt.Errorf("%w: the response carries more than it declared", errSnapshotFraming)
		}
		return nil
	}
	var firstRecords []protoreflect.Message
	message := first.ProtoReflect()
	for _, field := range path[:len(path)-1] {
		if !message.Has(field) {
			message = nil
			break
		}
		message = message.Get(field).Message()
	}
	if message != nil {
		values := message.Get(list).List()
		for index := range values.Len() {
			firstRecords = append(firstRecords, values.Get(index).Message())
		}
	}
	if err := account(firstRecords); err != nil {
		return err
	}
	for {
		part, err := next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		records, err := laterRecords(part.ProtoReflect(), path)
		if err != nil {
			return err
		}
		if err := account(records); err != nil {
			return err
		}
		if err := putRecords(first.ProtoReflect(), path, records, false); err != nil {
			return err
		}
	}
	if count != declared.records || size != declared.bytes {
		return fmt.Errorf("%w: the response carries %d records of %d bytes, not the %d records of %d bytes it declared",
			errSnapshotFraming, count, size, declared.records, declared.bytes)
	}
	return nil
}

// snapshot sends a batched read on the bulk connection and reassembles its
// list. A list larger than the client accepts is refused before it is read.
func (c *Client) snapshot(ctx context.Context, m *method, request, response proto.Message,
	open func(context.Context, ...grpc.CallOption) (grpc.ClientStream, error)) error {
	callCtx, evidence, err := c.prepare(ctx, m, request, c.bulkConn, MaxMessageBytes)
	if err != nil {
		return err
	}
	callCtx, cancel := context.WithCancel(callCtx)
	defer cancel()
	stream, err := open(callCtx)
	if err != nil {
		return decodeError(ctx, m, err, evidence)
	}
	// The list's size arrives in the response headers, before any record, so
	// an oversized list is refused without reading it.
	header, err := stream.Header()
	if err != nil {
		return decodeError(ctx, m, err, evidence)
	}
	declared, headerErr := parseSnapshotHeader(header)
	if headerErr == nil && declared.bytes > c.maxSnapshotBytes {
		return storagev1.TooLargeError{What: m.name + " response", Size: declared.bytes, Limit: c.maxSnapshotBytes}
	}
	if headerErr == nil && declared.records > c.maxSnapshotRecords {
		return storagev1.TooLargeError{What: m.name + " response records", Size: declared.records, Limit: c.maxSnapshotRecords}
	}
	if err := stream.RecvMsg(response); err != nil {
		// A refused call carries its status instead of a response.
		if errors.Is(err, io.EOF) {
			return c.responseError(m, errors.New("the response stream is empty"))
		}
		return decodeError(ctx, m, err, evidence)
	}
	if headerErr != nil {
		return c.responseError(m, headerErr)
	}
	path := batchedPathOf(response.ProtoReflect().Descriptor(), m.batched)
	err = reassembleRecords(response, path, declared, func() (proto.Message, error) {
		part := response.ProtoReflect().New().Interface()
		return part, stream.RecvMsg(part)
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errSnapshotFraming):
		return c.responseError(m, err)
	default:
		return decodeError(ctx, m, err, evidence)
	}
}

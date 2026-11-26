package wire

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/orka-agents/orka/pkg/storage/v1/wire/storagepb"
)

const MaxListPartBytes = MaxChunkBytes

var errListFraming = errors.New("the list stream breaks the framing rules")

func listFieldOf(desc protoreflect.MessageDescriptor) protoreflect.FieldDescriptor {
	for index := range desc.Fields().Len() {
		field := desc.Fields().Get(index)
		if marked, _ := proto.GetExtension(field.Options(), storagepb.E_List).(bool); marked {
			return field
		}
	}
	return nil
}

func splitList(response proto.Message, field protoreflect.FieldDescriptor) []proto.Message {
	items := response.ProtoReflect().Get(field).List()
	first := response.ProtoReflect().New()
	response.ProtoReflect().Range(func(populated protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if populated.Number() != field.Number() {
			first.Set(populated, value)
		}
		return true
	})
	parts := []proto.Message{first.Interface()}
	part, size := first.Mutable(field).List(), 0
	for index := range items.Len() {
		item := items.Get(index)
		itemSize := protowire.SizeTag(field.Number()) + protowire.SizeBytes(proto.Size(item.Message().Interface()))
		if size > 0 && size+itemSize > MaxListPartBytes {
			next := response.ProtoReflect().New()
			parts = append(parts, next.Interface())
			part, size = next.Mutable(field).List(), 0
		}
		part.Append(item)
		size += itemSize
	}
	return parts
}

func (s *Server) sendList(stream grpc.ServerStream, m *method, response proto.Message) error {
	ctx := stream.Context()
	if field, invalid := firstInvalidString(response.ProtoReflect()); invalid {
		return s.encodeError(ctx, m, fmt.Errorf("response field %s is not valid UTF-8", field))
	}
	field := listFieldOf(response.ProtoReflect().Descriptor())
	count := response.ProtoReflect().Get(field).List().Len()
	parts := splitList(response, field)
	for _, part := range parts {
		if size := proto.Size(part); size > MaxMessageBytes {
			return s.encodeError(ctx, m, fmt.Errorf("a list stream message of %d bytes exceeds the %d byte message limit",
				size, MaxMessageBytes))
		}
	}
	if err := stream.SetHeader(metadata.Pairs(HeaderListCount, strconv.Itoa(count))); err != nil {
		return status.Error(codes.Internal, "cannot set storage response headers")
	}
	for _, part := range parts {
		if err := stream.SendMsg(part); err != nil {
			return err
		}
	}
	return nil
}

func parseListCount(header metadata.MD) (int, error) {
	counts := header.Get(HeaderListCount)
	if len(counts) != 1 {
		return 0, fmt.Errorf("%w: the stream must declare its item count once", errListFraming)
	}
	count, err := strconv.Atoi(counts[0])
	if err != nil || count < 0 {
		return 0, fmt.Errorf("%w: the declared item count is invalid", errListFraming)
	}
	return count, nil
}

func appendListPart(list protoreflect.List, part protoreflect.Message, field protoreflect.FieldDescriptor) error {
	other := false
	part.Range(func(populated protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		other = populated.Number() != field.Number()
		return !other
	})
	if other {
		return fmt.Errorf("%w: a later message carries a field other than the list", errListFraming)
	}
	items := part.Get(field).List()
	if items.Len() == 0 {
		return fmt.Errorf("%w: a later message carries no part of the list", errListFraming)
	}
	for index := range items.Len() {
		list.Append(items.Get(index))
	}
	return nil
}

func (c *Client) list(ctx context.Context, m *method, request, response proto.Message,
	open func(context.Context, ...grpc.CallOption) (grpc.ClientStream, error)) error {
	callCtx, evidence, err := c.prepare(ctx, m, request, c.conn, MaxMessageBytes)
	if err != nil {
		return err
	}
	callCtx, cancel := context.WithCancel(callCtx)
	defer cancel()
	stream, err := open(callCtx)
	if err != nil {
		return decodeError(ctx, m, err, evidence)
	}
	header, err := stream.Header()
	if err != nil {
		return decodeError(ctx, m, err, evidence)
	}
	if err := stream.RecvMsg(response); err != nil {
		if errors.Is(err, io.EOF) {
			return c.responseError(m, errors.New("the response stream is empty"))
		}
		return c.callError(ctx, m, err, evidence, header)
	}
	if err := c.checkServed(m, header); err != nil {
		return err
	}
	declared, err := parseListCount(header)
	if err != nil {
		return c.responseError(m, err)
	}
	field := listFieldOf(response.ProtoReflect().Descriptor())
	list := response.ProtoReflect().Mutable(field).List()
	for {
		if list.Len() > declared {
			return c.responseError(m, fmt.Errorf("%w: the stream carries more items than it declared", errListFraming))
		}
		part := response.ProtoReflect().New()
		err := stream.RecvMsg(part.Interface())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return decodeError(ctx, m, err, evidence)
		}
		if err := appendListPart(list, part, field); err != nil {
			return c.responseError(m, err)
		}
	}
	if list.Len() != declared {
		return c.responseError(m, fmt.Errorf("%w: the stream carries %d items but declared %d", errListFraming, list.Len(), declared))
	}
	return nil
}

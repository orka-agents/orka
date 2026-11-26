package wire

import (
	"unicode/utf8"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// firstInvalidString returns the path of the first string in message that is
// not valid UTF-8. Protobuf refuses to encode such a string, so the client
// checks before sending and the server before answering.
func firstInvalidString(message protoreflect.Message) (string, bool) {
	var path string
	found := false
	walkFields(message, func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if field.Kind() != protoreflect.StringKind {
			return true
		}
		if !utf8.ValidString(value.String()) {
			path, found = string(field.FullName()), true
			return false
		}
		return true
	})
	return path, found
}

// hasUnknownFields reports whether message, or any message inside it,
// carries fields its descriptor does not define.
func hasUnknownFields(message protoreflect.Message) bool {
	if len(message.GetUnknown()) > 0 {
		return true
	}
	unknown := false
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		switch {
		case field.IsMap():
			if field.MapValue().Message() == nil {
				return true
			}
			value.Map().Range(func(_ protoreflect.MapKey, entry protoreflect.Value) bool {
				unknown = hasUnknownFields(entry.Message())
				return !unknown
			})
		case field.IsList():
			if field.Message() == nil {
				return true
			}
			list := value.List()
			for index := 0; index < list.Len() && !unknown; index++ {
				unknown = hasUnknownFields(list.Get(index).Message())
			}
		case field.Message() != nil:
			unknown = hasUnknownFields(value.Message())
		}
		return !unknown
	})
	return unknown
}

// walkFields calls visit for every populated scalar value in message and in
// every message inside it, including list elements and map keys and values.
// visit returns false to stop the walk.
func walkFields(message protoreflect.Message, visit func(protoreflect.FieldDescriptor, protoreflect.Value) bool) bool {
	keepGoing := true
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		switch {
		case field.IsMap():
			value.Map().Range(func(key protoreflect.MapKey, entry protoreflect.Value) bool {
				if keepGoing = visit(field.MapKey(), key.Value()); !keepGoing {
					return false
				}
				if field.MapValue().Message() != nil {
					keepGoing = walkFields(entry.Message(), visit)
				} else {
					keepGoing = visit(field.MapValue(), entry)
				}
				return keepGoing
			})
		case field.IsList():
			list := value.List()
			for index := 0; index < list.Len() && keepGoing; index++ {
				if field.Message() != nil {
					keepGoing = walkFields(list.Get(index).Message(), visit)
				} else {
					keepGoing = visit(field, list.Get(index))
				}
			}
		case field.Message() != nil:
			keepGoing = walkFields(value.Message(), visit)
		default:
			keepGoing = visit(field, value)
		}
		return keepGoing
	})
	return keepGoing
}

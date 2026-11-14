package storagefault

import "reflect"

// A provider can populate output through an input pointer (for example a
// generated Memory ID), or mutate nested maps/slices. Give it a private request
// only when the response will be lost. Healthy calls retain their original
// argument references. A lost response
// must not disclose fields a remote caller could not have received.
func cloneArgument[T any](argument T) T {
	value := reflect.ValueOf(argument)
	if !value.IsValid() {
		return argument
	}
	return cloneArgumentValue(value).Interface().(T)
}

func cloneArgumentValue(value reflect.Value) reflect.Value {
	switch value.Kind() {
	case reflect.Pointer:
		if value.IsNil() {
			return value
		}
		copy := reflect.New(value.Type().Elem())
		copy.Elem().Set(cloneArgumentValue(value.Elem()))
		return copy
	case reflect.Interface:
		if value.IsNil() {
			return value
		}
		copy := reflect.New(value.Type()).Elem()
		copy.Set(cloneArgumentValue(value.Elem()))
		return copy
	case reflect.Slice:
		if value.IsNil() {
			return value
		}
		copy := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for i := 0; i < value.Len(); i++ {
			copy.Index(i).Set(cloneArgumentValue(value.Index(i)))
		}
		return copy
	case reflect.Map:
		if value.IsNil() {
			return value
		}
		copy := reflect.MakeMapWithSize(value.Type(), value.Len())
		for iter := value.MapRange(); iter.Next(); {
			copy.SetMapIndex(iter.Key(), cloneArgumentValue(iter.Value()))
		}
		return copy
	case reflect.Struct, reflect.Array:
		copy := reflect.New(value.Type()).Elem()
		copy.Set(value)
		if value.Kind() == reflect.Array {
			for i := 0; i < value.Len(); i++ {
				copy.Index(i).Set(cloneArgumentValue(value.Index(i)))
			}
			return copy
		}
		for i := 0; i < value.NumField(); i++ {
			if value.Type().Field(i).IsExported() {
				copy.Field(i).Set(cloneArgumentValue(value.Field(i)))
			}
		}
		// Unexported fields, such as time.Time's immutable representation,
		// retain their exact value from the shallow copy above.
		return copy
	default:
		return value
	}
}

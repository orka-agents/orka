package storagefault

import (
	"reflect"
	"testing"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// TestCopyInfoSharesNothing fills every reference field of ProviderInfo and
// checks that copyInfo copies each one, so a field added later, such as a
// pointer to a dataset binding, cannot leak a Describe change into the
// provider's own report.
func TestCopyInfoSharesNothing(t *testing.T) {
	var info storagev1.ProviderInfo
	value := reflect.ValueOf(&info).Elem()
	for i := range value.NumField() {
		field := value.Field(i)
		name := value.Type().Field(i).Name
		switch field.Kind() {
		case reflect.Slice:
			field.Set(reflect.MakeSlice(field.Type(), 1, 1))
		case reflect.Pointer:
			field.Set(reflect.New(field.Type().Elem()))
			// copyInfo copies the value a pointer refers to, not what that
			// value refers to in turn.
			if elem := field.Type().Elem(); elem.Kind() == reflect.Struct {
				requireNoReferences(t, name, elem)
			}
		case reflect.Struct:
			if field.Type() != reflect.TypeFor[time.Time]() {
				requireNoReferences(t, name, field.Type())
			}
		case reflect.Map, reflect.Interface, reflect.Func, reflect.Chan:
			t.Fatalf("ProviderInfo.%s is a %s; teach copyInfo to copy it and this test to check it", name, field.Kind())
		}
	}
	copied := reflect.ValueOf(copyInfo(info))
	for i := range value.NumField() {
		switch value.Field(i).Kind() {
		case reflect.Slice, reflect.Pointer:
			require.NotEqualf(t, value.Field(i).Pointer(), copied.Field(i).Pointer(),
				"copyInfo shares ProviderInfo.%s with the provider's report", value.Type().Field(i).Name)
		}
	}
}

// requireNoReferences fails when the struct type typ holds a slice, pointer,
// map or other reference that copyInfo would share.
func requireNoReferences(t *testing.T, path string, typ reflect.Type) {
	t.Helper()
	for field := range typ.Fields() {
		fieldPath := path + "." + field.Name
		switch field.Type.Kind() {
		case reflect.Struct:
			if field.Type != reflect.TypeFor[time.Time]() {
				requireNoReferences(t, fieldPath, field.Type)
			}
		case reflect.Slice, reflect.Pointer, reflect.Map, reflect.Interface, reflect.Func, reflect.Chan:
			t.Fatalf("ProviderInfo.%s is a %s; teach copyInfo to copy it", fieldPath, field.Type.Kind())
		}
	}
}

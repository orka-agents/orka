package v1_test

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// catalogInterfaces are the interfaces whose methods the catalog lists.
var catalogInterfaces = map[string]reflect.Type{
	"StateProvider":           reflect.TypeFor[storagev1.StateProvider](),
	"ArtifactProvider":        reflect.TypeFor[storagev1.ArtifactProvider](),
	"OperationReceiptStore":   reflect.TypeFor[storagev1.OperationReceiptStore](),
	"ControllerEpochAdvancer": reflect.TypeFor[storagev1.ControllerEpochAdvancer](),
	"DatasetBindingStore":     reflect.TypeFor[storagev1.DatasetBindingStore](),
	"DatasetExporter":         reflect.TypeFor[storagev1.DatasetExporter](),
	"DatasetImporter":         reflect.TypeFor[storagev1.DatasetImporter](),
	"MigrationMarkerStore":    reflect.TypeFor[storagev1.MigrationMarkerStore](),
}

// readPrefixes are the name prefixes of methods that change nothing.
var readPrefixes = []string{"Get", "List", "Has", "Count", "Load", "Read", "Search", "Check", "Is", "Describe", "HealthCheck", "Export"}

// writesNamedLikeReads are writes whose names start with a read prefix.
var writesNamedLikeReads = []string{"GetMessages", "IsLocked"}

func providerMethod(t *testing.T, name string) reflect.Type {
	t.Helper()
	for _, iface := range catalogInterfaces {
		if method, ok := iface.MethodByName(name); ok {
			return method.Type
		}
	}
	t.Fatalf("no catalog interface has method %s", name)
	return nil
}

func TestMethodCatalogCoversEveryProviderMethod(t *testing.T) {
	servedBy := map[string][]string{}
	for ifaceName, iface := range catalogInterfaces {
		for method := range iface.Methods() {
			servedBy[method.Name] = append(servedBy[method.Name], ifaceName)
		}
	}
	catalog := storagev1.Methods()
	for name, interfaces := range servedBy {
		info, ok := catalog[name]
		if !ok {
			t.Errorf("provider method %s is missing from the method catalog in pkg/storage/v1/methods.go", name)
			continue
		}
		slices.Sort(interfaces)
		var want string
		switch {
		case slices.Equal(interfaces, []string{"StateProvider"}):
			want = storagev1.StateContract
		case slices.Equal(interfaces, []string{"ArtifactProvider"}):
			want = storagev1.ArtifactsContract
		}
		require.Equalf(t, want, info.Contract, "%s is served by %v", name, interfaces)
	}
	for name := range catalog {
		require.Containsf(t, servedBy, name, "catalog entry %s names no provider method", name)
	}
}

func TestMethodCatalogKinds(t *testing.T) {
	for name, info := range storagev1.Methods() {
		readName := slices.ContainsFunc(readPrefixes, func(prefix string) bool { return strings.HasPrefix(name, prefix) })
		switch info.Kind {
		case storagev1.MethodRead:
			require.Truef(t, readName, "read method %s should be named like a read", name)
			require.Emptyf(t, info.NaturalKey, "read method %s has no natural key", name)
		case storagev1.MethodWrite:
			require.Falsef(t, readName && !slices.Contains(writesNamedLikeReads, name),
				"write method %s is named like a read; classify it or list it in writesNamedLikeReads", name)
		case storagev1.MethodAdmin:
			require.Falsef(t, readName, "administrative method %s is named like a read", name)
			require.Emptyf(t, info.NaturalKey, "administrative method %s has no natural key", name)
		default:
			t.Errorf("method %s has kind %v", name, info.Kind)
		}
	}
	for name, kind := range map[string]storagev1.MethodKind{
		"AdvanceControllerEpoch":      storagev1.MethodAdmin,
		"SyncControllerEpochMirror":   storagev1.MethodAdmin,
		"ResolveOperation":            storagev1.MethodAdmin,
		"PruneOperationReceipts":      storagev1.MethodAdmin,
		"GetControllerEpochMirror":    storagev1.MethodRead,
		"BindDataset":                 storagev1.MethodAdmin,
		"DetachDatasetBinding":        storagev1.MethodAdmin,
		"UpdateBindingKeyFingerprint": storagev1.MethodAdmin,
		"CompleteDatasetBootstrap":    storagev1.MethodAdmin,
		"GetDatasetBinding":           storagev1.MethodRead,
		"GetMessages":                 storagev1.MethodWrite,
		"IsLocked":                    storagev1.MethodWrite,
		"Describe":                    storagev1.MethodRead,
		"SaveArtifact":                storagev1.MethodWrite,
		"ExportRecords":               storagev1.MethodRead,
		"GetImportProgress":           storagev1.MethodRead,
		"BeginImport":                 storagev1.MethodAdmin,
		"ImportRecords":               storagev1.MethodAdmin,
		"CompleteImport":              storagev1.MethodAdmin,
		"MarkDatasetMigrated":         storagev1.MethodAdmin,
	} {
		info, ok := storagev1.LookupMethod(name)
		require.True(t, ok, name)
		require.Equal(t, kind, info.Kind, name)
	}
	_, ok := storagev1.LookupMethod("NoSuchMethod")
	require.False(t, ok)
}

func TestMethodCatalogNaturalKeysResolve(t *testing.T) {
	for name, info := range storagev1.Methods() {
		if info.NaturalKey == "" {
			continue
		}
		method := providerMethod(t, name)
		for path := range strings.SplitSeq(info.NaturalKey, ", ") {
			segments := strings.Split(path, ".")
			require.Truef(t, strings.HasPrefix(segments[0], "$"), "%s natural key %q must start with $N", name, path)
			position, err := strconv.Atoi(strings.TrimPrefix(segments[0], "$"))
			require.NoErrorf(t, err, "%s natural key %q", name, path)
			require.Truef(t, position >= 1 && position < method.NumIn(), "%s natural key %q names no argument", name, path)
			current := method.In(position)
			for _, field := range segments[1:] {
				for current.Kind() == reflect.Pointer {
					current = current.Elem()
				}
				require.Equalf(t, reflect.Struct, current.Kind(), "%s natural key %q", name, path)
				next, ok := current.FieldByName(field)
				require.Truef(t, ok && next.IsExported(), "%s natural key %q: %s has no field %s", name, path, current, field)
				current = next.Type
			}
		}
	}
}

func TestMethodsReturnsACopy(t *testing.T) {
	catalog := storagev1.Methods()
	catalog["SaveResult"] = storagev1.MethodInfo{Kind: storagev1.MethodRead}
	delete(catalog, "GetResult")
	info, ok := storagev1.LookupMethod("SaveResult")
	require.True(t, ok)
	require.Equal(t, storagev1.MethodWrite, info.Kind)
	_, ok = storagev1.LookupMethod("GetResult")
	require.True(t, ok)
}

// TestWriteRequestsDigestEveryField fills every argument of every write with
// non-zero values and checks that the request digest covers each top-level
// field, including fields hidden from JSON.
func TestWriteRequestsDigestEveryField(t *testing.T) {
	contextType := reflect.TypeFor[context.Context]()
	for name, info := range storagev1.Methods() {
		if info.Kind != storagev1.MethodWrite {
			continue
		}
		t.Run(name, func(t *testing.T) {
			method := providerMethod(t, name)
			require.Equal(t, contextType, method.In(0), "the first argument is the context")
			args := make([]any, method.NumIn()-1)
			for i := range args {
				args[i] = filledValue(t, method.In(i+1), 0).Interface()
			}
			base := requestDigest(t, name, args...)
			for i := range args {
				value := reflect.ValueOf(args[i])
				structValue := value
				if structValue.Kind() == reflect.Pointer {
					structValue = structValue.Elem()
				}
				if structValue.Kind() != reflect.Struct || structValue.Type() == reflect.TypeFor[time.Time]() {
					changed := slices.Clone(args)
					changed[i] = reflect.Zero(value.Type()).Interface()
					require.NotEqualf(t, base, requestDigest(t, name, changed...), "argument %d is covered", i+1)
					continue
				}
				for f := range structValue.NumField() {
					copied := reflect.New(structValue.Type()).Elem()
					copied.Set(structValue)
					copied.Field(f).Set(reflect.Zero(copied.Field(f).Type()))
					changed := slices.Clone(args)
					if value.Kind() == reflect.Pointer {
						changed[i] = copied.Addr().Interface()
					} else {
						changed[i] = copied.Interface()
					}
					require.NotEqualf(t, base, requestDigest(t, name, changed...),
						"argument %d field %s is covered", i+1, structValue.Type().Field(f).Name)
				}
			}
		})
	}
}

// filledValue returns a value of type typ with every field set to a non-zero
// value.
func filledValue(t *testing.T, typ reflect.Type, depth int) reflect.Value {
	t.Helper()
	require.Lessf(t, depth, 12, "%s nests too deeply to fill", typ)
	value := reflect.New(typ).Elem()
	switch {
	case typ == reflect.TypeFor[time.Time]():
		value.Set(reflect.ValueOf(time.Date(2026, time.March, 4, 5, 6, 7, 123456789, time.FixedZone("AEST", 10*60*60))))
		return value
	case typ == reflect.TypeFor[json.RawMessage]():
		value.SetBytes([]byte(`{"filled":true}`))
		return value
	}
	switch typ.Kind() {
	case reflect.Bool:
		value.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value.SetInt(7)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		value.SetUint(7)
	case reflect.Float32, reflect.Float64:
		value.SetFloat(1.5)
	case reflect.String:
		value.SetString("filled")
	case reflect.Pointer:
		value.Set(filledValue(t, typ.Elem(), depth+1).Addr())
	case reflect.Slice:
		slice := reflect.MakeSlice(typ, 1, 1)
		slice.Index(0).Set(filledValue(t, typ.Elem(), depth+1))
		value.Set(slice)
	case reflect.Map:
		m := reflect.MakeMap(typ)
		m.SetMapIndex(filledValue(t, typ.Key(), depth+1), filledValue(t, typ.Elem(), depth+1))
		value.Set(m)
	case reflect.Interface:
		value.Set(reflect.ValueOf(map[string]any{"filled": true}))
	case reflect.Struct:
		for i := range typ.NumField() {
			field := typ.Field(i)
			require.Truef(t, field.IsExported(), "%s has unexported field %s, which a request digest cannot cover", typ, field.Name)
			value.Field(i).Set(filledValue(t, field.Type, depth+1))
		}
	default:
		t.Fatalf("%s cannot be part of a request", typ)
	}
	return value
}

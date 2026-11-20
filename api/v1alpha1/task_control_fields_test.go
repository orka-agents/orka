/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// taskSpecJSONFields lists TaskSpec fields by JSON name, in declaration order.
func taskSpecJSONFields(t *testing.T) []string {
	t.Helper()
	specType := reflect.TypeFor[TaskSpec]()
	fields := make([]string, 0, specType.NumField())
	for field := range specType.Fields() {
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			t.Fatalf("TaskSpec field %s has no JSON name", field.Name)
		}
		fields = append(fields, name)
	}
	return fields
}

// setNonZero gives a value a non-zero setting reachable through every kind
// TaskSpec uses.
func setNonZero(t *testing.T, value reflect.Value) {
	t.Helper()
	switch value.Kind() {
	case reflect.Pointer:
		value.Set(reflect.New(value.Type().Elem()))
		setNonZero(t, value.Elem())
	case reflect.String:
		value.SetString("x")
	case reflect.Bool:
		value.SetBool(true)
	case reflect.Int, reflect.Int32, reflect.Int64:
		value.SetInt(1)
	case reflect.Slice:
		value.Set(reflect.MakeSlice(value.Type(), 1, 1))
	case reflect.Map:
		value.Set(reflect.MakeMapWithSize(value.Type(), 1))
		key := reflect.New(value.Type().Key()).Elem()
		setNonZero(t, key)
		value.SetMapIndex(key, reflect.New(value.Type().Elem()).Elem())
	case reflect.Struct:
		for i := range value.NumField() {
			if value.Type().Field(i).IsExported() {
				setNonZero(t, value.Field(i))
				return
			}
		}
		t.Fatalf("struct %s has no exported field", value.Type())
	default:
		t.Fatalf("unhandled TaskSpec kind %s", value.Kind())
	}
}

// ExecutionInputs must clear exactly the control fields, so a new TaskSpec
// field is frozen by a soul binding unless it is deliberately listed.
func TestTaskSpecExecutionInputsClearOnlyControlFields(t *testing.T) {
	fields := taskSpecJSONFields(t)
	for _, control := range TaskControlFields {
		if !slices.Contains(fields, control) {
			t.Fatalf("control field %q is not a TaskSpec field", control)
		}
	}
	for i, name := range fields {
		t.Run(name, func(t *testing.T) {
			var spec TaskSpec
			setNonZero(t, reflect.ValueOf(&spec).Elem().Field(i))
			inputs := spec.ExecutionInputs()
			cleared := reflect.ValueOf(inputs).Field(i).IsZero()
			if control := slices.Contains(TaskControlFields, name); control != cleared {
				t.Fatalf("ExecutionInputs cleared %s=%t, want %t", name, cleared, control)
			}
			if !cleared && !reflect.DeepEqual(inputs, spec) {
				t.Fatalf("ExecutionInputs changed execution input %s", name)
			}
		})
	}
}

// The CRD rule must compare every execution input. Regenerate the expression
// from TaskSpec and require the marker to contain it exactly.
func TestTaskImmutabilityRuleCoversExecutionInputs(t *testing.T) {
	var comparisons []string
	for _, name := range taskSpecJSONFields(t) {
		if slices.Contains(TaskControlFields, name) {
			continue
		}
		comparisons = append(comparisons, fmt.Sprintf(
			"has(self.spec.%[1]s) == has(oldSelf.spec.%[1]s) && (!has(self.spec.%[1]s) || self.spec.%[1]s == oldSelf.spec.%[1]s)", name))
	}
	rule := "!has(oldSelf.status) || ((!has(oldSelf.status.agentExecutionBinding) && !has(oldSelf.status.soulBinding)) || " +
		"self.spec == oldSelf.spec || (!has(oldSelf.status.agentExecutionBinding) && (" + strings.Join(comparisons, " && ") + ")))"
	marker := "// +kubebuilder:validation:XValidation:rule=\"" + rule +
		"\",message=\"Task spec is immutable after execution authority is recorded\""
	if !strings.Contains(string(readTaskTypesSource(t)), marker) {
		t.Fatalf("Task immutability rule does not match TaskSpec; regenerate it as:\n%s", marker)
	}
}

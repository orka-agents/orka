/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package testutil

import (
	"fmt"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// AssertNoSpanText inspects the complete span payload for forbidden content.
func AssertNoSpanText(t *testing.T, span sdktrace.ReadOnlySpan, forbidden ...string) {
	t.Helper()
	if span == nil {
		t.Fatal("missing span")
	}
	check := func(value string) {
		t.Helper()
		for _, text := range forbidden {
			if strings.Contains(value, text) {
				t.Errorf("span %q contains forbidden test content", span.Name())
			}
		}
	}
	checkAttrs := func(attrs []attribute.KeyValue) {
		t.Helper()
		for _, attr := range attrs {
			check(string(attr.Key))
			check(fmt.Sprint(attr.Value.AsInterface()))
		}
	}
	check(span.Name())
	check(span.Status().Description)
	checkAttrs(span.Attributes())
	for _, event := range span.Events() {
		check(event.Name)
		checkAttrs(event.Attributes)
		for _, attr := range event.Attributes {
			if attr.Key == "exception.message" || attr.Key == "exception.stacktrace" {
				t.Errorf("span %q records %s", span.Name(), attr.Key)
			}
		}
	}
	for _, link := range span.Links() {
		checkAttrs(link.Attributes)
	}
}

// AssertSpanError checks that useful classification survives message removal.
func AssertSpanError(t *testing.T, span sdktrace.ReadOnlySpan, errorType string) {
	t.Helper()
	if span == nil {
		t.Fatal("missing error span")
	}
	if span.Status().Code != codes.Error || span.Status().Description != errorType {
		t.Errorf("status = %v, want error with class %q", span.Status(), errorType)
	}
	if got := AttributeMap(span)["error.type"].AsString(); got != errorType {
		t.Errorf("error.type = %q, want %q", got, errorType)
	}
	found := false
	for _, event := range span.Events() {
		if event.Name != "exception" {
			continue
		}
		found = true
		if len(event.Attributes) != 1 || event.Attributes[0].Key != "exception.type" || event.Attributes[0].Value.AsString() != errorType {
			t.Errorf("exception must contain only type %q", errorType)
		}
	}
	if !found {
		t.Error("missing exception event")
	}
}

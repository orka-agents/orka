package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/tools"
	"github.com/orka-agents/orka/internal/workerenv"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type unavailableToolReader struct {
	client.Client
	err error
}

func (r unavailableToolReader) Get(
	ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption,
) error {
	if _, tool := object.(*corev1alpha1.Tool); tool {
		return r.err
	}
	return r.Client.Get(ctx, key, object, opts...)
}

func TestNativeRemoteMCPPreservesLegacyStartupOnLookupFailure(t *testing.T) {
	for _, name := range []string{"file_read", "legacy"} {
		for _, failure := range []error{
			apierrors.NewForbidden(schema.GroupResource{Group: "core.orka.ai", Resource: "tools"}, name, errors.New("denied")),
			errors.New("temporarily unavailable"),
		} {
			t.Run(name+"/"+failure.Error(), func(t *testing.T) {
				f := newNativeRemoteFixture(t)
				reader := unavailableToolReader{Client: f.client, err: failure}
				loaded := loadCustomTools(t.Context(), reader, "team", []string{name})
				if _, err := prepareNativeRemoteTools(
					t.Context(), reader, "team", "task", []string{name}, loaded, f.executor,
				); err != nil {
					t.Fatalf("remote preparation broke legacy startup: %v", err)
				}
				loaded[name] = &corev1alpha1.Tool{
					ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team"},
					Spec:       corev1alpha1.ToolSpec{HTTP: &corev1alpha1.HTTPExecution{URL: "https://example.com"}},
				}
				if _, err := prepareNativeRemoteTools(
					t.Context(), reader, "team", "task", []string{name}, loaded, f.executor,
				); err != nil {
					t.Fatalf("remote preparation made a loaded legacy Tool reread fatal: %v", err)
				}
			})
		}
	}
}

func TestNativeRemoteMCPUnresolvedAliasIsNeitherAdvertisedNorCallable(t *testing.T) {
	f := newNativeRemoteFixture(t)
	reader := unavailableToolReader{Client: f.client, err: errors.New("temporarily unavailable")}
	enabled := []string{f.tool.Name}
	loaded := loadCustomTools(t.Context(), reader, "team", enabled)
	ctx, err := prepareNativeRemoteTools(t.Context(), reader, "team", "task", enabled, loaded, f.executor)
	if err != nil {
		t.Fatal("unknown backend lookup changed legacy best-effort startup")
	}
	advertised := buildLLMTools(enabled, loaded)
	if len(advertised) != 0 || len(advertisedToolNames(advertised)) != 0 {
		t.Fatal("unresolved alias was advertised or entered the invocation allowlist")
	}
	toolContext := &tools.ToolContext{Client: reader, Namespace: "team", TaskID: "task", TaskUID: "task-uid"}
	if _, err := executeNativeRemoteTool(ctx, toolContext, f.tool, json.RawMessage(`{}`)); err == nil {
		t.Fatal("unverified remote alias was callable")
	}
	if f.requests.Load() != 0 {
		t.Fatal("unresolved alias issued remote traffic")
	}
}

func TestNativeRemoteMCPKnownRemoteLookupFailureRemainsFatal(t *testing.T) {
	f := newNativeRemoteFixture(t)
	reader := unavailableToolReader{Client: f.client, err: errors.New("temporarily unavailable")}
	loaded := map[string]*corev1alpha1.Tool{"health": f.tool.DeepCopy()}
	if _, err := prepareNativeRemoteTools(
		t.Context(), reader, "team", "task", []string{"health"}, loaded, f.executor,
	); err == nil {
		t.Fatal("known remote lookup failure silently omitted the Tool")
	}
	if f.requests.Load() != 0 {
		t.Fatal("unavailable remote definition reached discovery")
	}
}

func TestNativeRemoteMCPRejectsLaunchUIDBeforeCredentials(t *testing.T) {
	for _, uid := range []string{"", "old-task-uid"} {
		t.Run(uid, func(t *testing.T) {
			f := newNativeRemoteFixture(t)
			t.Setenv(workerenv.TaskUID, uid)
			f.client = &nativeNoSecretReadClient{Client: f.client, t: t}
			if _, _, err := f.prepare(t); err == nil {
				t.Fatal("remote preparation accepted missing or stale controller-issued Task UID")
			}
			if f.requests.Load() != 0 {
				t.Fatal("launch UID mismatch issued protocol requests")
			}
		})
	}
}

func TestNativeRemoteMCPRejectsInvocationUIDBeforeCredentials(t *testing.T) {
	for _, uid := range []string{"", "other-task-uid"} {
		t.Run(uid, func(t *testing.T) {
			f := newNativeRemoteFixture(t)
			ctx, loaded, err := f.prepare(t)
			if err != nil {
				t.Fatal(err)
			}
			before := f.requests.Load()
			toolContext := &tools.ToolContext{
				Client: &nativeNoSecretReadClient{Client: f.client, t: t}, Namespace: "team", TaskID: "task", TaskUID: uid,
			}
			if _, err := executeNativeRemoteTool(ctx, toolContext, loaded["health"], json.RawMessage(`{}`)); err == nil {
				t.Fatal("invocation accepted missing or foreign Task UID")
			}
			if f.requests.Load() != before {
				t.Fatal("invocation UID mismatch issued protocol requests")
			}
		})
	}
}

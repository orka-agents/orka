/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/
package main

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func snapshotSecretTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func TestEnsureAgentExecutionSnapshotKeyMintsOnceAndReuses(t *testing.T) {
	scheme := snapshotSecretTestScheme(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "orka-agent-execution-snapshot", Namespace: "orka-system"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	opts := agentExecutionSnapshotSecretOptions{Name: "orka-agent-execution-snapshot", Key: "key"}

	first, err := ensureAgentExecutionSnapshotKey(context.Background(), c, c, "orka-system", opts, false)
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	if len(first) != 32 {
		t.Fatalf("minted key has %d bytes, want 32", len(first))
	}
	stored := &corev1.Secret{}
	ref := types.NamespacedName{Namespace: "orka-system", Name: opts.Name}
	if err := c.Get(context.Background(), ref, stored); err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(string(stored.Data["key"]))
	if err != nil || string(decoded) != string(first) {
		t.Fatalf("Secret item is not the base64 text of the minted key: %q (%v)", stored.Data["key"], err)
	}

	second, err := ensureAgentExecutionSnapshotKey(context.Background(), c, c, "orka-system", opts, true)
	if err != nil {
		t.Fatalf("second start: %v", err)
	}
	if string(second) != string(first) {
		t.Fatal("a later start must reuse the stored key, not mint a new one")
	}
}

func TestEnsureAgentExecutionSnapshotKeyAcceptsRawAndBase64Items(t *testing.T) {
	scheme := snapshotSecretTestScheme(t)
	raw := []byte(strings.Repeat("k", 32))
	items := map[string][]byte{
		"raw":    raw,
		"base64": []byte(base64.StdEncoding.EncodeToString(raw) + "\n"),
	}
	for name, item := range items {
		t.Run(name, func(t *testing.T) {
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "snapshot", Namespace: "orka-system"},
				Data:       map[string][]byte{"key": item},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
			got, err := ensureAgentExecutionSnapshotKey(context.Background(), c, c, "orka-system",
				agentExecutionSnapshotSecretOptions{Name: "snapshot", Key: "key"}, true)
			if err != nil || string(got) != string(raw) {
				t.Fatalf("key = %q, err = %v", got, err)
			}
		})
	}
}

func TestEnsureAgentExecutionSnapshotKeyFailsClosed(t *testing.T) {
	scheme := snapshotSecretTestScheme(t)
	opts := agentExecutionSnapshotSecretOptions{Name: "snapshot", Key: "key"}
	tests := []struct {
		name            string
		objects         []*corev1.Secret
		storePreexisted bool
		wantErr         string
	}{
		{
			name:    "Secret missing",
			wantErr: "does not exist",
		},
		{
			name: "store exists but key lost",
			objects: []*corev1.Secret{{
				ObjectMeta: metav1.ObjectMeta{Name: "snapshot", Namespace: "orka-system"},
			}},
			storePreexisted: true,
			wantErr:         "restore the Secret from backup",
		},
		{
			name: "malformed key item",
			objects: []*corev1.Secret{{
				ObjectMeta: metav1.ObjectMeta{Name: "snapshot", Namespace: "orka-system"},
				Data:       map[string][]byte{"key": []byte("too-short")},
			}},
			wantErr: "32 raw bytes",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := fake.NewClientBuilder().WithScheme(scheme)
			for _, object := range tt.objects {
				builder = builder.WithObjects(object)
			}
			c := builder.Build()
			_, err := ensureAgentExecutionSnapshotKey(context.Background(), c, c, "orka-system", opts,
				tt.storePreexisted)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
	incomplete := agentExecutionSnapshotSecretOptions{Name: "snapshot"}
	if err := validateAgentExecutionSnapshotSecretOptions(incomplete); err == nil {
		t.Fatal("an empty item key must be rejected")
	}
}

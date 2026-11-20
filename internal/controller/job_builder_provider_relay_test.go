/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/workerenv"
)

// A keyless Provider's slot carries no credential: the Job freezes which
// Provider the slot was built for, and the worker reaches it through the
// controller's relay. Key-based slots keep their Secret.
func TestJobBuilderRelaysKeylessProviderSlots(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = corev1alpha1.AddToScheme(scheme)

	primary := keylessTestProvider()
	primary.Namespace = defaultNS
	keyed := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "keyed", Namespace: defaultNS, UID: "uid-keyed"},
		Spec: corev1alpha1.ProviderSpec{
			Type:      corev1alpha1.ProviderTypeOpenAI,
			SecretRef: &corev1alpha1.ProviderSecretRef{Name: "keyed-secret"},
		},
	}
	keylessFallback := keylessTestProvider()
	keylessFallback.Name, keylessFallback.Namespace, keylessFallback.UID = "azure-fallback", defaultNS, "uid-fallback"

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(keyed, keylessFallback).Build()
	jb := NewJobBuilder(fc)
	jb.ControllerURL = testControllerURL

	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: defaultNS},
		Spec: corev1alpha1.AgentSpec{Model: &corev1alpha1.ModelConfig{Fallbacks: []corev1alpha1.ModelFallback{
			{ProviderRef: "keyed", Model: "gpt-small"},
			{ProviderRef: "azure-fallback", Model: "gpt-example"},
		}}},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: testTask, Namespace: defaultNS, UID: "uid-task"},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
			AI:   &corev1alpha1.AISpec{Prompt: "hello"},
			// A Task cannot claim a relay slot for itself.
			Env: []corev1.EnvVar{{Name: workerenv.AIProviderRelay, Value: `{"fallback-0":{"namespace":"default","name":"keyed","uid":"uid-keyed"}}`}},
		},
	}

	job, err := jb.Build(context.Background(), task, agent, primary)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	env := job.Spec.Template.Spec.Containers[0].Env
	var relayValues []string
	keyedFallbackKey := false
	for _, e := range env {
		switch e.Name {
		case workerenv.AIProviderRelay:
			relayValues = append(relayValues, e.Value)
		case workerenv.OpenAIAPIKey, workerenv.FallbackAPIKeyKey(1):
			t.Fatalf("keyless slot got a credential env var %s", e.Name)
		case workerenv.FallbackAPIKeyKey(0):
			keyedFallbackKey = e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && e.ValueFrom.SecretKeyRef.Name == "keyed-secret"
		}
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && e.ValueFrom.SecretKeyRef.Name != "keyed-secret" {
			t.Fatalf("unexpected Secret reference in %s: %+v", e.Name, e.ValueFrom.SecretKeyRef)
		}
	}
	if !keyedFallbackKey {
		t.Fatal("the key-based fallback lost its Secret")
	}
	if len(relayValues) != 1 {
		t.Fatalf("%s set %d times, want once", workerenv.AIProviderRelay, len(relayValues))
	}
	slots, err := workerenv.ParseProviderRelay(relayValues[0])
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]workerenv.ProviderRelayTarget{
		workerenv.ProviderRelayPrimary:     {Namespace: defaultNS, Name: "azure", UID: "uid-1"},
		workerenv.ProviderRelayFallback(1): {Namespace: defaultNS, Name: "azure-fallback", UID: "uid-fallback"},
	}
	if len(slots) != len(want) {
		t.Fatalf("relay slots = %+v, want %+v", slots, want)
	}
	for slot, target := range want {
		if slots[slot] != target {
			t.Fatalf("relay slot %s = %+v, want %+v", slot, slots[slot], target)
		}
	}
}

func TestJobBuilderReservesTheProviderRelayWithoutKeylessProviders(t *testing.T) {
	jb := setupJobBuilder()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: testTask, Namespace: defaultNS, UID: "uid-task"},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
			AI:   &corev1alpha1.AISpec{Prompt: "hello"},
			Env:  []corev1.EnvVar{{Name: workerenv.AIProviderRelay, Value: `{"primary":{"namespace":"default","name":"x","uid":"y"}}`}},
		},
	}
	job, err := jb.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	count := 0
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		if e.Name == workerenv.AIProviderRelay {
			count++
			if e.Value != "" {
				t.Fatalf("%s = %q, want empty", e.Name, e.Value)
			}
		}
	}
	if count != 1 {
		t.Fatalf("%s set %d times, want once", workerenv.AIProviderRelay, count)
	}
}

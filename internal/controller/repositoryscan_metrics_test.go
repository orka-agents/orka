/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/metrics"
	"github.com/orka-agents/orka/internal/security"
)

func TestAnalysisTaskAnnotationsRecordsIsolationOutcomes(t *testing.T) {
	metrics.SecurityIsolationOutcomesTotal.Reset()

	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	supported := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "codex", Namespace: "ns"},
		Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{
			Type: corev1alpha1.AgentRuntimeCodex,
		}},
	}
	unsupported := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "opencode", Namespace: "ns"},
		Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{
			Type: corev1alpha1.AgentRuntimeOpencode,
		}},
	}
	reconciler := &RepositoryScanReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(supported, unsupported).Build(),
	}
	newScan := func(policy, agent string) *corev1alpha1.RepositoryScan {
		return &corev1alpha1.RepositoryScan{
			ObjectMeta: metav1.ObjectMeta{Name: "repo", Namespace: "ns"},
			Spec: corev1alpha1.RepositoryScanSpec{
				AnalysisIsolationPolicy: policy,
				AnalysisAgentRef:        corev1alpha1.AgentReference{Name: agent},
			},
		}
	}

	annotations, err := reconciler.analysisTaskAnnotations(context.Background(), newScan("legacy", ""))
	if err != nil || annotations["orka.ai/security-isolation-status"] != security.IsolationStatusLegacy {
		t.Fatalf("legacy annotations/error = %#v/%v", annotations, err)
	}
	annotations, err = reconciler.analysisTaskAnnotations(context.Background(), newScan("require-hardened", supported.Name))
	if err != nil || annotations["orka.ai/security-isolation-status"] != security.IsolationStatusHardened {
		t.Fatalf("hardened annotations/error = %#v/%v", annotations, err)
	}
	annotations, err = reconciler.analysisTaskAnnotations(context.Background(), newScan("prefer-hardened", unsupported.Name))
	if err != nil || annotations["orka.ai/security-isolation-status"] != security.IsolationStatusFallback {
		t.Fatalf("fallback annotations/error = %#v/%v", annotations, err)
	}
	if _, err := reconciler.analysisTaskAnnotations(context.Background(), newScan("require-hardened", unsupported.Name)); err == nil {
		t.Fatal("require-hardened unsupported runtime error = nil")
	}

	for _, expected := range []struct {
		policy  string
		outcome string
	}{
		{policy: "legacy", outcome: "legacy"},
		{policy: "require-hardened", outcome: "hardened"},
		{policy: "prefer-hardened", outcome: "fallback"},
		{policy: "require-hardened", outcome: "failed"},
	} {
		if got := metrics.CounterVecValue(metrics.SecurityIsolationOutcomesTotal, expected.policy, expected.outcome); got != 1 {
			t.Fatalf("isolation outcome %s/%s = %v, want 1", expected.policy, expected.outcome, got)
		}
	}
}

func TestRecordSecurityInventoryMetricsUsesBoundedClasses(t *testing.T) {
	metrics.SecurityInventoryEntriesTotal.Reset()
	metrics.SecurityInventoryReasonClassesTotal.Reset()

	artifact := &security.ReviewSlicesArtifact{
		SchemaVersion: security.SchemaVersionReviewSlicesV2,
		InventorySummary: &security.MapperInventorySummary{
			TruncatedEntries: 7,
			Truncated:        true,
			Reason:           security.MapperCoverageReasonInventoryEntryLimit,
		},
		DiscoveredFiles: []security.MapperFileInventoryEntry{
			{Path: "main.go", Disposition: security.MapperDispositionReviewable, Reason: "supported-reviewable-file"},
			{Path: "vendor", Disposition: security.MapperDispositionExcluded, Reason: "dependency-directory"},
			{Path: "private", Disposition: security.MapperDispositionExcluded, Reason: "repository/ns/task/path"},
		},
		ReviewableFiles: []security.MapperFileInventoryEntry{
			{Path: "main.go", Disposition: security.MapperDispositionAssigned, Reason: "assigned-to-review-slice"},
			{Path: "orphan.go", Disposition: security.MapperDispositionOmitted, Reason: "no-deterministic-review-slice"},
		},
		OmittedFiles: []security.MapperFileInventoryEntry{
			{Path: "vendor", Disposition: security.MapperDispositionExcluded, Reason: "dependency-directory"},
			{Path: "orphan.go", Disposition: security.MapperDispositionOmitted, Reason: "no-deterministic-review-slice"},
			{Path: "main.go", Disposition: security.MapperDispositionOmitted, Reason: "context-reference-cap"},
		},
	}

	recordSecurityInventoryMetrics(artifact)

	for _, expected := range []struct {
		disposition string
		count       float64
	}{
		{disposition: "eligible", count: 1},
		{disposition: "excluded", count: 2},
		{disposition: "assigned", count: 1},
		{disposition: "omitted", count: 2},
		{disposition: "truncated", count: 7},
	} {
		if got := metrics.CounterVecValue(metrics.SecurityInventoryEntriesTotal, expected.disposition); got != expected.count {
			t.Fatalf("inventory disposition %s = %v, want %v", expected.disposition, got, expected.count)
		}
	}
	for _, expected := range []struct {
		disposition string
		reason      string
		count       float64
	}{
		{disposition: "eligible", reason: "eligible", count: 1},
		{disposition: "excluded", reason: "dependency", count: 1},
		{disposition: "excluded", reason: "other", count: 1},
		{disposition: "assigned", reason: "assigned", count: 1},
		{disposition: "omitted", reason: "unassigned", count: 1},
		{disposition: "omitted", reason: "truncated", count: 1},
		{disposition: "truncated", reason: "truncated", count: 7},
	} {
		if got := metrics.CounterVecValue(metrics.SecurityInventoryReasonClassesTotal, expected.disposition, expected.reason); got != expected.count {
			t.Fatalf("inventory reason %s/%s = %v, want %v", expected.disposition, expected.reason, got, expected.count)
		}
	}
}

func TestRecordSecurityInventoryMetricsCountsTruncatedOmissionRecords(t *testing.T) {
	metrics.SecurityInventoryEntriesTotal.Reset()
	metrics.SecurityInventoryReasonClassesTotal.Reset()

	recordSecurityInventoryMetrics(&security.ReviewSlicesArtifact{
		SchemaVersion: security.SchemaVersionReviewSlicesV2,
		InventorySummary: &security.MapperInventorySummary{
			TruncatedEntries: 0,
			OmissionRecords: &security.MapperOmissionRecordSummary{
				TruncatedRecords: 5,
				Truncated:        true,
			},
			Truncated: true,
			Reason:    security.MapperCoverageReasonInventoryEntryLimit,
		},
	})

	if got := metrics.CounterVecValue(metrics.SecurityInventoryEntriesTotal, "truncated"); got != 5 {
		t.Fatalf("truncated inventory entries = %v, want 5 omission records", got)
	}
	if got := metrics.CounterVecValue(metrics.SecurityInventoryReasonClassesTotal, "truncated", "truncated"); got != 5 {
		t.Fatalf("truncated inventory reason class = %v, want 5 omission records", got)
	}
}

func TestRecordSecurityInventoryMetricsIgnoresSchemaV1InventoryFields(t *testing.T) {
	metrics.SecurityInventoryEntriesTotal.Reset()
	metrics.SecurityInventoryReasonClassesTotal.Reset()
	recordSecurityInventoryMetrics(&security.ReviewSlicesArtifact{
		SchemaVersion:    security.SchemaVersionReviewSlices,
		InventorySummary: &security.MapperInventorySummary{TruncatedEntries: 999, Truncated: true},
		DiscoveredFiles:  []security.MapperFileInventoryEntry{{Path: "forged.go", Disposition: "eligible", Reason: "forged"}},
	})
	if got := metrics.CounterVecValue(metrics.SecurityInventoryEntriesTotal, "eligible"); got != 0 {
		t.Fatalf("schema-v1 eligible inventory metric = %v, want 0", got)
	}
	if got := metrics.CounterVecValue(metrics.SecurityInventoryEntriesTotal, "truncated"); got != 0 {
		t.Fatalf("schema-v1 truncated inventory metric = %v, want 0", got)
	}
}

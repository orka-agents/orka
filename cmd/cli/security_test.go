package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	securitybundle "github.com/orka-agents/orka/internal/security/bundle"
)

func TestSecurityScanDocumentOutputsFullDocument(t *testing.T) {
	for _, document := range []string{"bundle", "coverage"} {
		t.Run(document, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wantPath := "/api/v1/security/repositories/repo-1/scans/run-1/" + document
				if r.Method != http.MethodGet || r.URL.Path != wantPath {
					t.Fatalf("request = %s %s, want GET %s", r.Method, r.URL.Path, wantPath)
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id":       document + "-1",
					"manifest": map[string]any{"targetSHA": "abc123"},
				})
			}))
			defer server.Close()

			var output bytes.Buffer
			root := newRootCmd()
			root.SetOut(&output)
			root.SetArgs([]string{
				"--server", server.URL,
				"security", "scan", document, "repo-1", "run-1",
			})
			if err := root.Execute(); err != nil {
				t.Fatalf("Execute() error = %v", err)
			}

			var result map[string]any
			if err := json.Unmarshal(output.Bytes(), &result); err != nil {
				t.Fatalf("document output = %q, want JSON: %v", output.String(), err)
			}
			if result["id"] != document+"-1" {
				t.Fatalf("id = %#v, want %q", result["id"], document+"-1")
			}
			manifest, _ := result["manifest"].(map[string]any)
			if manifest["targetSHA"] != "abc123" {
				t.Fatalf("manifest = %#v, want targetSHA", manifest)
			}
		})
	}
}

func TestSecurityScanDocumentAcceptsOutputFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"bundle-1","contentDigest":"sha256:test"}`))
	}))
	defer server.Close()

	var output bytes.Buffer
	root := newRootCmd()
	root.SetOut(&output)
	root.SetArgs([]string{
		"--server", server.URL,
		"security", "scan", "bundle", "repo-1", "run-1", "-o", "yaml",
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := output.String(); got != "contentDigest: sha256:test\nid: bundle-1\n" {
		t.Fatalf("YAML output = %q", got)
	}
}

func TestSecurityFindingHistoryForwardsPagination(t *testing.T) {
	tests := []struct {
		action string
		kind   string
	}{
		{action: "occurrences"},
		{action: "decisions"},
		{action: "assessments", kind: "validation"},
	}

	for _, tt := range tests {
		t.Run(tt.action, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wantPath := "/api/v1/security/findings/finding-1/" + tt.action
				if r.Method != http.MethodGet || r.URL.Path != wantPath {
					t.Fatalf("request = %s %s, want GET %s", r.Method, r.URL.Path, wantPath)
				}
				if got := r.URL.Query().Get("limit"); got != "17" {
					t.Fatalf("limit = %q, want 17", got)
				}
				if got := r.URL.Query().Get("cursor"); got != "next-page" {
					t.Fatalf("cursor = %q, want next-page", got)
				}
				if got := r.URL.Query().Get("kind"); got != tt.kind {
					t.Fatalf("kind = %q, want %q", got, tt.kind)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"items":[],"metadata":{"continue":"later"}}`))
			}))
			defer server.Close()

			args := []string{
				"--server", server.URL,
				"security", "finding", tt.action, "finding-1",
				"--limit", strconv.Itoa(17), "--continue", "next-page",
			}
			if tt.kind != "" {
				args = append(args, "--kind", tt.kind)
			}
			var output bytes.Buffer
			root := newRootCmd()
			root.SetOut(&output)
			root.SetArgs(args)
			if err := root.Execute(); err != nil {
				t.Fatalf("Execute() error = %v", err)
			}

			var result map[string]any
			if err := json.Unmarshal(output.Bytes(), &result); err != nil {
				t.Fatalf("history output = %q, want JSON: %v", output.String(), err)
			}
		})
	}
}

func TestSecurityScanQualityCheckVerifiesEnvelopeAndQuality(t *testing.T) {
	for _, validated := range []bool{false, true} {
		t.Run(map[bool]string{false: "sealed", true: "validated"}[validated], func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			const repository = "repo-1"
			const runID = "scan-1"
			response := securityBundleResponse(t, repository, runID)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wantPath := "/api/v1/security/repositories/repo-1/scans/scan-1/bundle"
				if r.Method != http.MethodGet || r.URL.Path != wantPath {
					t.Fatalf("request = %s %s, want GET %s", r.Method, r.URL.Path, wantPath)
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(response)
			}))
			defer server.Close()

			args := []string{"--server", server.URL, "security", "scan", "check", repository, runID}
			if validated {
				args = append(args, "--validated")
			}
			var output bytes.Buffer
			root := newRootCmd()
			root.SetOut(&output)
			root.SetArgs(args)
			if err := root.Execute(); err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			want := "Security scan bundle is sealed"
			if validated {
				want = "Security scan satisfies validated quality"
			}
			if !strings.Contains(output.String(), want) {
				t.Fatalf("output = %q, want %q", output.String(), want)
			}
		})
	}
}

func TestSecurityScanQualityCheckRejectsTamperedBundle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	response := securityBundleResponse(t, "repo-1", "scan-1")
	response["contentDigest"] = "sha256:" + strings.Repeat("0", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	root := newRootCmd()
	root.SetArgs([]string{"--server", server.URL, "security", "scan", "check", "repo-1", "scan-1"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "bundle verification failed") {
		t.Fatalf("Execute() error = %v, want bundle verification failure", err)
	}
}

func securityBundleResponse(t *testing.T, repository, runID string) map[string]any {
	t.Helper()
	started := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	completed := started.Add(time.Minute)
	sealed := completed.Add(time.Second)
	publicRunID := runID
	bundle, err := securitybundle.Build(securitybundle.Input{
		Manifest: securitybundle.ManifestInput{
			SchemaVersion: securitybundle.SchemaVersion,
			Repository: securitybundle.RepositoryIdentity{
				Provider: "github", RepositoryID: "repository-1", RepoURL: "https://github.com/example/repo.git",
			},
			Target: securitybundle.TargetSnapshot{
				CommitSHA: strings.Repeat("a", 40), TreeDigest: "sha256:" + strings.Repeat("b", 64),
				TargetID: "target-1", ReceiptID: "target-receipt-1", ReceiptDigest: "sha256:" + strings.Repeat("c", 64),
			},
			ThreatModel: securitybundle.ThreatModelInput{Version: "1", Content: "Repository trust boundaries."},
			Quality: securitybundle.QualitySummary{
				InventoryCoverage: "complete", CandidateCoverage: "complete", Coverage: "complete",
				ValidationScope: "all", ValidationExecution: "complete", AttackPathExecution: "complete",
				AnalysisAttestation: "tool-observed", TargetVerification: "verified",
				Authorization: "verified", Isolation: "hardened",
			},
			Versions:      securitybundle.ComponentVersions{Schema: "security-bundle-v1", Controller: "controller-v1"},
			OccurrenceIDs: []string{}, AssessmentIDs: []string{}, StageReceiptIDs: []string{}, EvidenceReceiptIDs: []string{},
			Metadata: map[string]string{},
			Run: securitybundle.RunEnvelope{
				RunUID: "run_" + strings.Repeat("d", 64), PublicRunID: &publicRunID,
				Namespace: "default", RepositoryScanName: repository, RepositoryScanUID: "repository-scan-uid-1",
				RepositoryScanGeneration: 1, StartedAt: started, CompletedAt: &completed, SealedAt: sealed,
			},
		},
		Findings: securitybundle.FindingsInput{
			SchemaVersion: securitybundle.SchemaVersion,
			Findings:      []securitybundle.Finding{},
			Metadata:      map[string]string{},
		},
		Coverage: securitybundle.CoverageInput{
			SchemaVersion:   securitybundle.SchemaVersion,
			InventoryStatus: "complete",
			CandidateStatus: "complete",
			CoverageStatus:  "complete",
			Inventory:       []securitybundle.InventoryCoverageEntry{},
			Candidates:      []securitybundle.CandidateCoverageEntry{},
			Stages:          []securitybundle.StageCoverageEntry{},
			Metadata:        map[string]string{},
		},
		Evidence: []securitybundle.EvidenceBlobInput{},
	}, securitybundle.DefaultLimits())
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	return map[string]any{
		"id": "bundle-1", "scanRunID": runID, "runUID": "run_" + strings.Repeat("d", 64),
		"version": securitybundle.SchemaVersion, "manifest": json.RawMessage(bundle.ManifestJSON),
		"findings": json.RawMessage(bundle.FindingsJSON), "coverage": json.RawMessage(bundle.CoverageJSON),
		"evidence": bundle.Evidence, "contentDigest": bundle.Roots.ContentDigest,
		"runReceiptDigest": bundle.Roots.RunReceiptDigest, "sealedAt": sealed,
	}
}

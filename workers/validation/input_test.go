//go:build linux

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	pv "github.com/orka-agents/orka/internal/patchverification"
)

func inputFixture(t *testing.T) pv.PodInput {
	t.Helper()
	var archive bytes.Buffer
	tarball := tar.NewWriter(&archive)
	source := []byte("frozen source\n")
	if err := tarball.WriteHeader(&tar.Header{Name: "source.txt", Mode: 0644, Size: int64(len(source))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarball.Write(source); err != nil {
		t.Fatal(err)
	}
	if err := tarball.Close(); err != nil {
		t.Fatal(err)
	}
	checkFile := []byte("#!/bin/sh\nprintf 'healthy\\n'\n")
	manifest := pv.Manifest{
		Version: pv.SchemaVersion, Action: pv.ValidateReport,
		Problem: "bounded worker fixture", Scope: []string{"worker protocol"},
		Sources: pv.Sources{Repository: "/fixture/repository", Original: pv.SourceIdentity{
			Commit: strings.Repeat("a", 40), Tree: strings.Repeat("b", 40), ArchiveDigest: pv.Digest(archive.Bytes()),
		}},
		Environment: pv.Environment{
			Image: "fixture/tool@" + pv.Digest([]byte("image")), ImageID: pv.Digest([]byte("config")),
			Platform: "linux/" + runtime.GOARCH, Profile: pv.Offline,
			Dependencies: map[string]string{"orka.kubernetes.policy": pv.KubernetesPolicyVersion},
		},
		Files: []pv.FrozenFile{{Path: "run", Content: checkFile, Digest: pv.Digest(checkFile), Executable: true}},
		Checks: []pv.Check{{
			ID: "case-one", Kind: pv.Reproduction, Command: []string{"/checks/run"}, TimeoutSeconds: 1,
			Healthy: pv.Expectation{Stdout: "healthy\n"}, Failure: pv.Expectation{Stdout: "broken\n"},
		}},
	}
	var err error
	manifest.ReportDigest, err = pv.StableReportDigest(manifest.Problem, manifest.Scope)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := pv.NewRunBinding(manifest, "attempt-one", "task-original", "")
	if err != nil {
		t.Fatal(err)
	}
	return pv.PodInput{
		Version: pv.PodProtocolVersion, Manifest: manifest, Binding: binding,
		Side: pv.Original, CheckID: "case-one", Archive: archive.Bytes(),
	}
}

func gzipFixture(t *testing.T, content []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestDecodePodInput(t *testing.T) {
	input := inputFixture(t)
	content, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	compressed := gzipFixture(t, content)
	decoded, err := decodeInput(bytes.NewReader(compressed))
	if err != nil || !reflect.DeepEqual(input, decoded) {
		t.Fatalf("input changed on decode: %v", err)
	}
	corrupted := bytes.Clone(compressed)
	corrupted[len(corrupted)-5] ^= 1
	for name, value := range map[string][]byte{
		"plain json":          content,
		"compressed overflow": bytes.Repeat([]byte("x"), pv.MaxPodInputBytes+1),
		"bad checksum":        corrupted,
		"trailing bytes":      append(bytes.Clone(compressed), 'x'),
		"extra gzip member":   append(bytes.Clone(compressed), compressed...),
		"extra json document": gzipFixture(t, append(bytes.Clone(content), []byte("\n{}")...)),
		"unknown field":       gzipFixture(t, append([]byte(`{"unknown":true,`), content[1:]...)),
		"partial stream":      compressed[:len(compressed)-2],
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeInput(bytes.NewReader(value)); err == nil {
				t.Fatal("invalid compressed input was accepted")
			}
		})
	}
	input.Archive[0] ^= 1
	if _, err := pv.ValidatePodInput(input); err == nil {
		t.Fatal("source digest mismatch was accepted")
	}
}

func TestDecodePodInputExpansionLimit(t *testing.T) {
	compressed := gzipFixture(t, bytes.Repeat([]byte(" "), maxDecodedInputBytes+1))
	if len(compressed) >= pv.MaxPodInputBytes {
		t.Fatal("fixture does not exercise the decompression limit")
	}
	if _, err := decodeInput(bytes.NewReader(compressed)); err == nil {
		t.Fatal("oversized decompressed input was accepted")
	}
}

func TestReadPodInputRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "input")
	if err := os.WriteFile(target, []byte("input"), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := readInput(alias); err == nil {
		t.Fatal("symlinked input was accepted")
	}
}

func TestReportRejectsCredentialLikeOutputBeforeTransport(t *testing.T) {
	report := newReport(inputFixture(t), "pod-one")
	content := []byte("password=synthetic-fixture-value")
	digest := pv.Digest(content)
	report.Evidence.Blobs[digest] = content
	report.Evidence.Observation.StdoutDigest = digest
	report.Evidence.Observation.StdoutBytes = len(content)
	var output bytes.Buffer
	if err := writeReport(&output, report); err != nil {
		t.Fatal(err)
	}
	var decoded pv.PodReport
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.TaskUID != report.TaskUID || decoded.Evidence.Observation.SetupError == "" {
		t.Fatal("rejected output lost its binding or did not invalidate the check")
	}
	for _, blob := range decoded.Evidence.Blobs {
		if len(blob) != 0 {
			t.Fatal("credential-like bytes entered the Pod report transport")
		}
	}
	if decoded.Evidence.Observation.StdoutBytes != 0 || decoded.Evidence.Observation.StdoutDigest != pv.Digest(nil) {
		t.Fatal("unsafe capture metadata was not cleared")
	}
}

func TestReportIsOneBoundedDocument(t *testing.T) {
	report := newReport(inputFixture(t), "pod-one")
	childText := []byte("{\"executed\":true}\nextra child output\n")
	capture := newCapture("", nil)
	_, _ = capture.Write(childText)
	output := captureEvidence(&report.Evidence, capture)
	report.Evidence.Observation.StdoutDigest, report.Evidence.Observation.StdoutBytes = output.Digest, output.Bytes
	var buffer bytes.Buffer
	if err := writeReport(&buffer, report); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&buffer)
	var decoded pv.PodReport
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if decoder.Decode(new(any)) != io.EOF || decoded.Evidence.Observation.Executed ||
		!bytes.Equal(decoded.Evidence.Blobs[output.Digest], childText) {
		t.Fatal("child output influenced supervisor facts or escaped the report")
	}
	report.Evidence.Blobs["oversized"] = bytes.Repeat([]byte("x"), pv.MaxPodReportBytes)
	buffer.Reset()
	if err := writeReport(&buffer, report); err != nil || buffer.Len() > pv.MaxPodReportBytes {
		t.Fatalf("bounded failure report unavailable: %v", err)
	}
	if json.Unmarshal(buffer.Bytes(), &decoded) != nil || decoded.Evidence.Observation.SetupError == "" ||
		!decoded.Evidence.Observation.OutputTruncated {
		t.Fatal("oversized report did not fail closed")
	}
}

func TestReportSurvivesSetupFailure(t *testing.T) {
	input := inputFixture(t)
	report := newReport(input, "pod-one")
	t.Setenv("ORKA_VALIDATION_TASK_UID", "another-task")
	runBoundInput(context.Background(), input, input.Manifest.Checks[0], &report)
	if report.Evidence.Observation.SetupError == "" || report.Evidence.Observation.Executed ||
		report.TaskUID != input.Binding.OriginalTaskID ||
		pv.ValidateObservation(input.Manifest, input.Binding, report.Evidence.Observation) != nil {
		t.Fatal("setup failure lost its valid binding or invented execution")
	}
}

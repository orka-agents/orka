//go:build linux

package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"

	pv "github.com/orka-agents/orka/internal/patchverification"
)

const maxDecodedInputBytes = (pv.MaxProvenanceBytes+2)/3*4 + pv.MaxManifestBytes + 16<<10

var uidPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

func decodeInput(reader io.Reader) (pv.PodInput, error) {
	var input pv.PodInput
	compressed, err := io.ReadAll(io.LimitReader(reader, pv.MaxPodInputBytes+1))
	if err != nil || len(compressed) == 0 || len(compressed) > pv.MaxPodInputBytes {
		return input, errors.New("validation input exceeds its compressed limit")
	}
	raw := bytes.NewReader(compressed)
	zipped, err := gzip.NewReader(raw)
	if err != nil {
		return input, errors.New("validation input is not a gzip bundle")
	}
	zipped.Multistream(false)
	decoded, readErr := io.ReadAll(io.LimitReader(zipped, maxDecodedInputBytes+1))
	closeErr := zipped.Close()
	if readErr != nil || closeErr != nil || len(decoded) > maxDecodedInputBytes || raw.Len() != 0 {
		return input, errors.New("validation bundle is truncated, oversized, or has trailing data")
	}
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF {
		return pv.PodInput{}, errors.New("validation bundle is not one protocol document")
	}
	return input, nil
}

func readInput(filename string) (pv.PodInput, error) {
	info, err := os.Lstat(filename)
	if err != nil || !info.Mode().IsRegular() || info.Size() > pv.MaxPodInputBytes {
		return pv.PodInput{}, errors.New("validation input is not a bounded regular file")
	}
	file, err := os.Open(filename)
	if err != nil {
		return pv.PodInput{}, errors.New("validation input is unavailable")
	}
	defer func() { _ = file.Close() }()
	return decodeInput(file)
}

func newReport(input pv.PodInput, podUID string) pv.PodReport {
	taskUID, tree := input.Binding.OriginalTaskID, input.Manifest.Sources.Original.Tree
	if input.Side == pv.Patched {
		taskUID, tree = input.Binding.PatchedTaskID, input.Manifest.Sources.Patched.Tree
	}
	return pv.PodReport{
		Version: pv.PodProtocolVersion,
		TaskUID: taskUID,
		PodUID:  podUID,
		Evidence: pv.ExecutionEvidence{
			Observation: pv.Observation{
				RunID: input.Binding.RunID, AttemptID: input.Binding.AttemptID,
				TaskID: taskUID, ManifestDigest: input.Binding.ManifestDigest,
				Side: input.Side, CheckID: input.CheckID, SourceTree: tree,
				ImageID: input.Manifest.Environment.ImageID, Origin: "runner",
				PodUID: podUID,
			},
			Blobs: make(map[string][]byte),
		},
	}
}

func writeReport(writer io.Writer, report pv.PodReport) error {
	for _, blob := range report.Evidence.Blobs {
		if pv.ContainsCredentialMaterial(blob) {
			clearReportCapture(&report, "execution output contained credential-like material")
			break
		}
	}
	content, err := json.Marshal(report)
	if err != nil || len(content)+1 > pv.MaxPodReportBytes {
		// Keep the valid binding even if an internal reporting limit is exceeded.
		report.Evidence.Blobs = make(map[string][]byte)
		observation := &report.Evidence.Observation
		observation.SetupError = "validation report exceeded its output limit"
		observation.OutputTruncated = true
		observation.StdoutDigest, observation.StderrDigest = "", ""
		observation.StdoutBytes, observation.StderrBytes = 0, 0
		observation.ServiceOutputs = nil
		content, err = json.Marshal(report)
	}

	if err != nil || len(content)+1 > pv.MaxPodReportBytes {
		return errors.New("validation report could not be encoded")
	}
	content = append(content, '\n')
	n, err := writer.Write(content)
	if err != nil || n != len(content) {
		return errors.New("validation report could not be written completely")
	}
	return nil
}

func clearReportCapture(report *pv.PodReport, reason string) {
	empty := pv.Digest(nil)
	report.Evidence.Blobs = map[string][]byte{empty: {}}
	observation := &report.Evidence.Observation
	observation.SetupError = reason
	observation.StdoutDigest, observation.StderrDigest = empty, empty
	observation.StdoutBytes, observation.StderrBytes = 0, 0
	for id := range observation.ServiceOutputs {
		observation.ServiceOutputs[id] = pv.CapturedOutput{Digest: empty}
	}
}

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/orka-agents/orka/pkg/oms/conformance"
	"github.com/orka-agents/orka/pkg/oms/protocol"
)

const (
	maxCheckpointBytes = 1 << 20
	phaseCheck         = "check"
	phasePrepare       = "prepare"
	phaseVerify        = "verify"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv))
}

func run(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	defaults := conformance.DefaultBinding()
	flags := flag.NewFlagSet("orka-oms-conformance", flag.ContinueOnError)
	flags.SetOutput(stderr)
	endpoint := flags.String("endpoint", "", "OMS adapter base URL")
	tokenEnv := flags.String("token-env", "ORKA_OMS_BEARER_TOKEN", "environment variable containing the bearer token")
	phase := flags.String("phase", phaseCheck, "conformance phase: check, prepare, or verify")
	stateFile := flags.String("state-file", "", "checkpoint path required for prepare/verify")
	timeout := flags.Duration("timeout", 15*time.Second, "per-request timeout")
	overallTimeout := flags.Duration("overall-timeout", 5*time.Minute, "overall phase timeout")
	disableProxy := flags.Bool("disable-proxy", true, "disable HTTP proxy use for adapter traffic")
	insecureLoopbackOnly := flags.Bool(
		"insecure-loopback-only", false,
		"allow plaintext HTTP only to a literal loopback address (local testing only)",
	)
	providerCommitGapProof := flags.Bool(
		"provider-commit-gap-proof", false,
		"opt in to the adapter's authenticated conformance-only provider-commit/local-receipt failpoint",
	)
	runID := flags.String("run-id", "", "optional lowercase run identifier")
	clusterID := flags.String("cluster-id", defaults.ClusterID, "binding cluster ID")
	namespaceUID := flags.String("namespace-uid", defaults.NamespaceUID, "binding namespace UID")
	backendUID := flags.String("backend-uid", defaults.BackendUID, "binding backend UID")
	storeName := flags.String("store-name", "conformance-store", "operator-selected store name")
	authorityEpoch := flags.Uint64("authority-epoch", defaults.AuthorityEpoch, "binding authority epoch")
	routingEpoch := flags.Uint64("routing-epoch", defaults.RoutingEpoch, "initial binding routing epoch")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*endpoint) == "" {
		_, _ = fmt.Fprintln(stderr, "--endpoint is required")
		return 2
	}
	if *phase != phaseCheck && *phase != phasePrepare && *phase != phaseVerify {
		_, _ = fmt.Fprintln(stderr, "--phase must be check, prepare, or verify")
		return 2
	}
	if (*phase == phasePrepare || *phase == phaseVerify) && strings.TrimSpace(*stateFile) == "" {
		_, _ = fmt.Fprintln(stderr, "--state-file is required for prepare and verify")
		return 2
	}
	token := strings.TrimSpace(getenv(*tokenEnv))
	if token == "" {
		_, _ = fmt.Fprintf(stderr, "%s is required\n", *tokenEnv)
		return 2
	}
	binding := protocol.Binding{
		ClusterID: strings.TrimSpace(*clusterID), NamespaceUID: strings.TrimSpace(*namespaceUID),
		BackendUID: strings.TrimSpace(*backendUID), AuthorityEpoch: *authorityEpoch,
		RoutingEpoch: *routingEpoch, StoreUUID: defaults.StoreUUID,
	}
	binding.TenantID = protocol.DeriveTenantID(binding.ClusterID, binding.NamespaceUID)
	authorizationValue := "Bearer " + token
	target := conformance.Target{
		BaseURL: *endpoint, AuthorizationValue: authorizationValue, Binding: binding,
		StoreName: strings.TrimSpace(*storeName),
		RunID:     strings.TrimSpace(*runID), Timeout: *timeout, DisableProxy: *disableProxy,
		InsecureLoopbackOnly: *insecureLoopbackOnly, ProviderCommitGapProof: *providerCommitGapProof,
	}
	ctx, cancel := context.WithTimeout(context.Background(), *overallTimeout)
	defer cancel()

	var result conformance.CheckResult
	switch *phase {
	case phaseCheck:
		result = conformance.Check(ctx, target)
	case phasePrepare:
		checkpoint, prepared := conformance.Prepare(ctx, target)
		result = prepared
		if result.Passed {
			if err := writeCheckpoint(*stateFile, checkpoint); err != nil {
				_, _ = fmt.Fprintf(stderr, "write checkpoint: %v\n", err)
				return 2
			}
		}
	case phaseVerify:
		checkpoint, err := readCheckpoint(*stateFile)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "read checkpoint: %v\n", err)
			return 2
		}
		result = conformance.VerifyAfterRestart(ctx, target, checkpoint)
	}
	if err := writeResult(stdout, result, token); err != nil {
		_, _ = fmt.Fprintf(stderr, "write result: %v\n", err)
		return 2
	}
	if !result.Passed {
		return 1
	}
	return 0
}

func writeResult(writer io.Writer, result conformance.CheckResult, token string) error {
	return json.NewEncoder(writer).Encode(conformance.SanitizeCheckResult(result, token))
}

func writeCheckpoint(path string, checkpoint conformance.Checkpoint) error {
	if err := conformance.ValidateCheckpoint(checkpoint); err != nil {
		return err
	}
	data, err := json.MarshalIndent(checkpoint, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > maxCheckpointBytes {
		return errors.New("checkpoint exceeds size limit")
	}
	cleanPath := filepath.Clean(path)
	if err := os.MkdirAll(filepath.Dir(cleanPath), 0o750); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(cleanPath), ".oms-checkpoint-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName) //nolint:errcheck
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, cleanPath)
}

func readCheckpoint(path string) (conformance.Checkpoint, error) {
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		return conformance.Checkpoint{}, err
	}
	defer file.Close() //nolint:errcheck
	data, err := io.ReadAll(io.LimitReader(file, maxCheckpointBytes+1))
	if err != nil {
		return conformance.Checkpoint{}, err
	}
	if len(data) == 0 || len(data) > maxCheckpointBytes {
		return conformance.Checkpoint{}, errors.New("checkpoint size is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var checkpoint conformance.Checkpoint
	if err := decoder.Decode(&checkpoint); err != nil {
		return conformance.Checkpoint{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return conformance.Checkpoint{}, errors.New("checkpoint contains trailing JSON")
		}
		return conformance.Checkpoint{}, err
	}
	if err := conformance.ValidateCheckpoint(checkpoint); err != nil {
		return conformance.Checkpoint{}, err
	}
	return checkpoint, nil
}

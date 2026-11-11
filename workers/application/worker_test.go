//go:build linux

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/stretchr/testify/require"
)

func workerTestInput(t *testing.T) pv.ApplicationWorkerInput {
	t.Helper()
	yes, err := pv.ApplicationAPIExpectation(pv.ApplicationAPIValue{Exists: true, Found: true, Value: []byte(`true`)})
	require.NoError(t, err)
	no, err := pv.ApplicationAPIExpectation(pv.ApplicationAPIValue{Exists: true, Found: true, Value: []byte(`false`)})
	require.NoError(t, err)
	manifest := pv.Manifest{
		Version: pv.SchemaVersion, Action: pv.ValidateReport,
		Problem: "application accepts unsafe input", Scope: []string{"status after reconciliation"},
		Sources: pv.Sources{Repository: "https://github.com/example/public-source",
			Original: pv.SourceIdentity{
				Commit: strings.Repeat("a", 40), Tree: strings.Repeat("b", 40),
				ArchiveDigest: pv.Digest([]byte("complete source")),
			}},
		Environment: pv.Environment{
			Image: "example/toolchain@" + pv.Digest([]byte("toolchain")), ImageID: pv.Digest([]byte("toolchain-config")),
			Platform: "linux/amd64", Profile: pv.ApplicationRuntimeProfile,
			Dependencies: map[string]string{pv.ApplicationPolicyKey: pv.ApplicationPolicyVersion,
				pv.ApplicationObserverImageKey:   "example/worker@" + pv.Digest([]byte("worker")),
				pv.ApplicationObserverImageIDKey: pv.Digest([]byte("worker-config"))},
		},
		Application: &pv.ApplicationPlan{Version: pv.ApplicationProtocolVersion,
			Build: pv.ApplicationBuild{Profile: pv.ApplicationBuildGoPackage, Package: "./cmd/server", GoVersion: "go1.27.2"},
			Runtime: pv.ApplicationRuntime{
				Image: "example/runtime@" + pv.Digest([]byte("runtime")), ImageID: pv.Digest([]byte("runtime-config")),
			},
			Permissions: []pv.ApplicationPermission{{Resources: []string{"services/status"}, Verbs: []string{"patch"}}}},
		Checks: []pv.Check{
			{ID: "unsafe", Kind: pv.Reproduction, TimeoutSeconds: 10, Healthy: no, Failure: yes,
				Application: &pv.ApplicationCheck{Version: pv.ApplicationProtocolVersion,
					Kubernetes: &pv.ApplicationKubernetesCheck{
						APIVersion: "v1", Resource: "services", Name: "case-unsafe", Pointer: "/status/accepted",
					}}},
			{ID: "normal", Kind: pv.Normal, TimeoutSeconds: 10, Healthy: yes, Failure: no,
				Application: &pv.ApplicationCheck{Version: pv.ApplicationProtocolVersion,
					Resources: []json.RawMessage{json.RawMessage(
						`{"apiVersion":"v1","kind":"Service","metadata":{"name":"case-normal"},` +
							`"spec":{"selector":{"orka.ai/application-target":"subject"},` +
							`"ports":[{"port":8080,"targetPort":8080,"protocol":"TCP"}]}}`,
					)},
					Kubernetes: &pv.ApplicationKubernetesCheck{
						APIVersion: "v1", Resource: "services", Name: "case-normal", Pointer: "/status/accepted",
						NormalValue: []byte("true"),
					}}},
		},
	}
	manifest.ReportDigest, err = pv.StableReportDigest(manifest.Problem, manifest.Scope)
	require.NoError(t, err)
	manifest.ApplicationSourceMetadata = &pv.ApplicationSourceMetadata{Version: pv.ApplicationProtocolVersion,
		Original: pv.ApplicationArchiveMetadata{ArchiveDigest: manifest.Sources.Original.ArchiveDigest,
			ArchiveBytes: 4096, ContentBytes: 100, Entries: 2}}
	binding, err := pv.NewRunBinding(manifest, "attempt", "original-task", "")
	require.NoError(t, err)
	input := pv.ApplicationWorkerInput{Version: pv.ApplicationProtocolVersion, Manifest: manifest, Binding: binding,
		Side: pv.Original, TaskUID: "build-task",
		Target: pv.ApplicationTarget{ClusterUID: "target-cluster", Namespace: "run-original", NamespaceUID: "namespace-uid"}}
	require.NoError(t, pv.ValidateApplicationWorkerInput(input))
	return input
}

func TestBuildCommandIsFixedOfflineAndDoesNotInheritCompilerEnvironment(t *testing.T) {
	input := workerTestInput(t)
	t.Setenv("GOFLAGS", "-toolexec=/untrusted/compiler")
	t.Setenv("CGO_ENABLED", "1")
	t.Setenv("GOPROXY", "https://untrusted.invalid")
	args, environment, err := buildCommand(input)
	require.NoError(t, err)
	require.Equal(t, []string{
		"build", "-mod=vendor", "-trimpath", "-buildvcs=false", "-p=2", "-o", "/artifacts/application", "./cmd/server",
	}, args)
	for _, fixed := range []string{
		"CGO_ENABLED=0", "GOTOOLCHAIN=local", "GOENV=off", "GOFLAGS=", "GOWORK=off", "GOPROXY=off", "GOSUMDB=off",
	} {
		require.Contains(t, environment, fixed)
	}
	require.NotContains(t, strings.Join(environment, "\n"), "untrusted")
	for _, bad := range []string{
		"./...", "../outside", "/absolute", "-toolexec=bad", "./cmd/../../bad", "./cmd/server;sh",
	} {
		input := workerTestInput(t)
		input.Manifest.Application.Build.Package = bad
		_, _, err := buildCommand(input)
		require.Error(t, err)
	}
}

func TestWorkerRejectsUnknownCommandsAndUnsafePathsBeforeExecution(t *testing.T) {
	for _, args := range [][]string{
		{"shell", "--input", pv.ApplicationInputPath},
		{"build", "--input", "../outside"},
		{"subject", "--input", "/etc/passwd"},
		{"observe", "--input", pv.ApplicationInputPath, "--command", "/bin/sh"},
		{"install", "--destination", "/host/worker"},
		{"git", "/bin/sh", "-c", "echo unsafe"},
	} {
		var output bytes.Buffer
		require.Error(t, run(t.Context(), args, &output))
		require.Empty(t, output.Bytes())
	}
}

func TestWorkerInputRejectsUnknownBuildInstructionsAndTrailingData(t *testing.T) {
	input := workerTestInput(t)
	content, err := json.Marshal(input)
	require.NoError(t, err)
	_, err = pv.DecodeApplicationWorkerInput(bytes.NewReader(content))
	require.NoError(t, err)
	bad := bytes.Replace(
		content, []byte(`"profile":"go-package-v1"`), []byte(`"profile":"go-package-v1","command":["sh","-c","unsafe"]`), 1,
	)
	_, err = pv.DecodeApplicationWorkerInput(bytes.NewReader(bad))
	require.Error(t, err)
	_, err = pv.DecodeApplicationWorkerInput(strings.NewReader(string(content) + "{}"))
	require.Error(t, err)
}

type applicationTransportFunc func(*http.Request) (*http.Response, error)

func (fn applicationTransportFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestObserverTransportCannotRedirectCredentialsOrUseProxyEndpoints(t *testing.T) {
	// Generate dummy userinfo at runtime rather than storing credential-shaped fixtures.
	userinfo := &url.URL{Scheme: "https", Host: "10.0.0.1:6443", Path: "/api", User: url.User(rand.Text())}
	credentials := &url.URL{Scheme: "https", Host: "10.0.0.1:6443", User: url.UserPassword(rand.Text(), rand.Text())}
	called := 0
	transport := &targetTransport{
		host: "10.0.0.1:6443",
		base: applicationTransportFunc(func(*http.Request) (*http.Response, error) {
			called++
			return &http.Response{StatusCode: 200}, nil
		}),
	}
	for _, endpoint := range []string{"http://10.0.0.1:6443/api", "https://other.invalid/api", userinfo.String()} {
		request, err := http.NewRequest(http.MethodGet, endpoint, nil)
		require.NoError(t, err)
		_, err = transport.RoundTrip(request)
		require.Error(t, err)
	}

	require.Zero(t, called)
	request, err := http.NewRequest(http.MethodGet, "https://10.0.0.1:6443/api", nil)
	require.NoError(t, err)
	_, err = transport.RoundTrip(request)
	require.NoError(t, err)
	require.Equal(t, 1, called)
	config, err := targetConfig("https://10.0.0.1:6443")
	require.NoError(t, err)
	require.Equal(t, "/target/token", config.BearerTokenFile)
	require.Equal(t, "/target/ca.crt", config.CAFile)
	require.Empty(t, config.BearerToken)
	require.False(t, config.Insecure)
	for _, endpoint := range []string{
		"http://10.0.0.1:6443", "https://api.example.invalid", "https://10.0.0.1:6443/other", credentials.String(),
	} {
		_, err := targetConfig(endpoint)
		require.Error(t, err)
	}
}

func TestApplicationAbsoluteDeadlineCannotBeResetOrExtended(t *testing.T) {
	for _, invalid := range []string{"not-a-timestamp", "2020-01-01T00:00:00Z"} {
		_, _, err := applicationDeadline(t.Context(), invalid)
		require.Error(t, err)
	}
	earlier := time.Now().Add(time.Minute)
	parent, cancel := context.WithDeadline(t.Context(), earlier)
	defer cancel()
	child, stop, err := applicationDeadline(parent, time.Now().Add(time.Hour).Format(time.RFC3339Nano))
	require.NoError(t, err)
	defer stop()
	actual, ok := child.Deadline()
	require.True(t, ok)
	require.Equal(t, earlier, actual)
	frozen := time.Now().Add(30 * time.Second).UTC()
	child, stop, err = applicationDeadline(parent, frozen.Format(time.RFC3339Nano))
	require.NoError(t, err)
	defer stop()
	actual, ok = child.Deadline()
	require.True(t, ok)
	require.True(t, frozen.Equal(actual))
	cancelled, cancelNow := context.WithCancel(t.Context())
	cancelNow()
	_, _, err = applicationDeadline(cancelled, "")
	require.ErrorIs(t, err, context.Canceled)
}

package main

import (
	"context"
	"flag"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"

	"github.com/orka-agents/orka/internal/remediation/contract"
)

func TestApplicationRemediationRequiresExplicitEnablementAndCompleteOptions(t *testing.T) {
	for _, options := range []remediationOptions{
		{Backend: "unknown"},
		{Backend: string(contract.ApplicationRuntime)},
		{
			Enabled: true, AgentName: "native",
			Application: remediationApplicationOptions{WorkerImage: "example.invalid/worker"},
		},
		{Enabled: true, AgentName: "native", Backend: string(contract.ApplicationRuntime)},
	} {
		service, backend, err := setupRemediation(context.Background(), nil, nil, nil, nil, options)
		require.Error(t, err)
		require.Nil(t, service)
		require.Nil(t, backend, "incomplete application config must fail before touching a Kubernetes manager")
	}
}

func TestApplicationFlagsDoNotEnableOrChangeLegacySourceProfile(t *testing.T) {
	flags := flag.NewFlagSet("synthetic", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	options := remediationApplicationOptions{}
	bindRemediationApplicationFlags(flags, &options)
	require.False(t, options.configured())
	require.NoError(t, flags.Parse([]string{
		"--remediation-application-context=isolated",
		"--remediation-application-go-version=go1.27.2",
		"--remediation-application-platform=linux/arm64",
		"--remediation-application-policy-file=operator-policy.json",
		"--remediation-application-runtime-class=isolated-runtime",
		"--remediation-application-api-server=https://198.51.100.40:443",
		"--remediation-application-source-endpoint=github.com=192.0.2.31",
		"--remediation-application-source-endpoint=api.github.com=192.0.2.32",
		"--remediation-application-runtime-qualification-file=runtime-qualification.json",
	}))
	require.True(t, options.configured())
	require.Equal(t, "isolated", options.Context)
	require.Equal(t, "go1.27.2", options.GoVersion)
	require.Equal(t, "linux/arm64", options.Platform)
	require.Equal(t, "operator-policy.json", options.PolicyFile)
	require.Equal(t, "isolated-runtime", options.RuntimeClassName)
	require.Equal(t, "https://198.51.100.40:443", options.APIServer)
	require.Len(t, options.SourceEndpoints, 2)
	require.Equal(t, "github.com", options.SourceEndpoints[0].Host)
	require.Equal(t, "192.0.2.31", options.SourceEndpoints[0].Address)
	require.Equal(t, "runtime-qualification.json", options.RuntimeQualificationFile)
}

func TestApplicationBackendConfigurationKeepsTargetSeparateAndBounded(t *testing.T) {
	options := remediationOptions{
		Namespace: "private",
		Application: remediationApplicationOptions{
			NodeSelector:     "remediation.orka.ai/application-qualified=true",
			NetworkCanary:    "192.0.2.10:8080",
			TargetClusterUID: "target-uid", ControlClusterUID: "control-uid", GoVersion: "go1.27.2",
		},
	}
	target := &rest.Config{
		Host:            "https://192.0.2.20:6443",
		TLSClientConfig: rest.TLSClientConfig{CAData: []byte("public-test-ca")},
	}
	config, err := applicationBackendConfig(options, target)
	require.NoError(t, err)
	require.Equal(t, options.Namespace, config.Namespace)
	require.Equal(t, target.Host, config.TargetAPIServer)
	require.Equal(t, target.CAData, config.TargetCAData)
	require.Equal(t, "target-uid", config.TargetClusterUID)
	require.Equal(t, "control-uid", config.ControlClusterUID)
	require.Equal(t, contract.MaxRunDuration, config.RunTimeout)
	require.Equal(t, "go1.27.2", config.GoVersion)
	options.Application.APIServer = "https://198.51.100.40:443"
	options.Application.RuntimeClassName = "isolated-runtime"
	config, err = applicationBackendConfig(options, target)
	require.NoError(t, err)
	require.Equal(t, options.Application.APIServer, config.TargetAPIServer)
	require.Equal(t, "https://192.0.2.20:6443", target.Host,
		"worker routing must not mutate the control-side target client")
	require.Equal(t, options.Application.RuntimeClassName, config.RuntimeClassName)
	require.NoError(t, options.Application.SourceEndpoints.Set("github.com=192.0.2.31"))
	config, err = applicationBackendConfig(options, target)
	require.NoError(t, err)
	require.Len(t, config.SourceEndpoints, 1)
	require.Equal(t, "github.com", config.SourceEndpoints[0].Host)
	options.Application.SourceEndpoints[0].Address = "192.0.2.99"
	require.Equal(t, "192.0.2.31", config.SourceEndpoints[0].Address,
		"configuration must not alias mutable flag storage")
	options.Application.NetworkCanary = "unqualified.invalid:8080"
	_, err = applicationBackendConfig(options, target)
	require.Error(t, err)
	options.Application.NetworkCanary = "192.0.2.10:8080"
	options.Application.NodeSelector = "unqualified"
	_, err = applicationBackendConfig(options, target)
	require.Error(t, err)
}

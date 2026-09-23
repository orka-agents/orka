package main

import (
	"context"
	"flag"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestProviderAuthProxyConcurrencyEnvironment(t *testing.T) {
	const helper = "ORKA_TEST_PROVIDER_PROXY_HELP"
	const name = "ORKA_PROVIDER_AUTH_PROXY_MAX_CONCURRENT_REQUESTS"
	if os.Getenv(helper) == "1" {
		flag.CommandLine = flag.NewFlagSet("provider-auth-proxy", flag.ExitOnError)
		os.Args = []string{"provider-auth-proxy", "-help"}
		main()
		return
	}

	for _, scenario := range []struct {
		name, value string
		valid       bool
	}{
		{name: "unset", valid: true},
		{name: "zero", value: "0", valid: true},
		{name: "negative", value: "-1", valid: true},
		{name: "positive", value: "17", valid: true},
		{name: "plus sign", value: "+10"},
		{name: "zero padded", value: "010"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProviderAuthProxyConcurrencyEnvironment$")
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(entry, "ORKA_PROVIDER_AUTH_PROXY_") && !strings.HasPrefix(entry, helper+"=") {
					command.Env = append(command.Env, entry)
				}
			}
			command.Env = append(command.Env, helper+"=1", name+"="+scenario.value)
			output, err := command.CombinedOutput()
			if scenario.valid {
				if err != nil || !strings.Contains(string(output), "Usage of provider-auth-proxy:") {
					t.Fatalf("startup help with concurrency %q: %v\n%s", scenario.value, err, output)
				}
			} else if err == nil || !strings.Contains(string(output), "invalid "+name) {
				t.Fatalf("startup accepted malformed concurrency %q: %v\n%s", scenario.value, err, output)
			}
		})
	}
}

func TestNormalizeProxyConfigConcurrencyDefault(t *testing.T) {
	for _, scenario := range []struct {
		value, want int
	}{
		{value: 0, want: defaultMaxConcurrentRequests},
		{value: -1, want: defaultMaxConcurrentRequests},
		{value: 17, want: 17},
	} {
		config, _, err := normalizeProxyConfig(proxyConfig{
			UpstreamBaseURL:       "http://vekil.example",
			MaxConcurrentRequests: scenario.value,
		})
		if err != nil || config.MaxConcurrentRequests != scenario.want {
			t.Fatalf("concurrency %d normalized to %d, %v; want %d", scenario.value, config.MaxConcurrentRequests, err, scenario.want)
		}
	}
}

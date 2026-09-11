package publisher

import (
	"slices"
	"strings"
	"testing"
)

const testProxyPassword = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ-_"

func TestNormalizeProxyEnvironment(t *testing.T) {
	rawProxy := "http://orka-publisher:" + testProxyPassword + "@orka-scm-egress-proxy.orka-system.svc:8080"
	environment, err := NormalizeProxyEnvironment(
		rawProxy,
		"localhost,127.0.0.1,::1,.svc,.cluster.local",
	)
	if err != nil {
		t.Fatalf("NormalizeProxyEnvironment: %v", err)
	}
	variables := environment.Variables()
	for _, expected := range []string{
		"HTTPS_PROXY=" + rawProxy,
		"https_proxy=" + rawProxy,
		"NO_PROXY=localhost,127.0.0.1,::1,.svc,.cluster.local",
		"no_proxy=localhost,127.0.0.1,::1,.svc,.cluster.local",
	} {
		if !slices.Contains(variables, expected) {
			t.Fatalf("proxy variables %q do not include %q", variables, expected)
		}
	}
}

func TestNormalizeProxyEnvironmentRejectsUnsafeBypassesAndCredentials(t *testing.T) {
	validProxy := "http://orka-publisher:" + testProxyPassword + "@proxy.orka-system.svc:8080"
	cases := map[string]struct {
		proxyURL string
		noProxy  string
	}{
		"missing authentication": {proxyURL: "http://proxy.orka-system.svc:8080", noProxy: ".svc"},
		"wrong username": {
			proxyURL: "http://other:" + testProxyPassword + "@proxy.orka-system.svc:8080",
			noProxy:  ".svc",
		},
		"public bypass":   {proxyURL: validProxy, noProxy: "github.com"},
		"wildcard bypass": {proxyURL: validProxy, noProxy: "*"},
		"private bypass":  {proxyURL: validProxy, noProxy: "10.0.0.1"},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := NormalizeProxyEnvironment(testCase.proxyURL, testCase.noProxy)
			if err == nil {
				t.Fatal("NormalizeProxyEnvironment accepted unsafe configuration")
			}
			if strings.Contains(err.Error(), testProxyPassword) {
				t.Fatal("validation error exposed the proxy credential")
			}
		})
	}
}

func TestPublisherCommandEnvironmentIncludesOnlyExplicitProxy(t *testing.T) {
	rawProxy := "http://orka-publisher:" + testProxyPassword + "@proxy.orka-system.svc:8080"
	proxyEnvironment, err := NormalizeProxyEnvironment(rawProxy, ".svc,.cluster.local")
	if err != nil {
		t.Fatal(err)
	}
	publisher := &Publisher{proxyEnvironment: proxyEnvironment, trustedPath: "/usr/bin:/bin"}
	environment := publisher.commandEnvironment(&sandbox{
		home: "/tmp/home", xdg: "/tmp/xdg", template: "/tmp/template",
	}, nil)
	for _, expected := range proxyEnvironment.Variables() {
		if !slices.Contains(environment, expected) {
			t.Fatalf("command environment does not include %q", expected)
		}
	}
}

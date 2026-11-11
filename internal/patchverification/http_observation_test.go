package patchverification

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func httpExpectation(t *testing.T, status int, body string) Expectation {
	t.Helper()
	expectation, err := HTTPExpectation(status, body)
	if err != nil {
		t.Fatal(err)
	}
	return expectation
}

func httpCheck(t *testing.T) Check {
	t.Helper()
	return Check{
		ID: "external-case", Kind: Reproduction, TimeoutSeconds: 2,
		HTTP:    &HTTPCheck{Version: HTTPCheckVersion, ServerCommand: []string{"/checks/run"}, Path: "/quantity?value=-1"},
		Healthy: httpExpectation(t, 200, "healthy"),
		Failure: httpExpectation(t, 500, "broken"),
	}
}

func httpManifest(t *testing.T) Manifest {
	t.Helper()
	manifest := testManifest()
	manifest.Environment.Profile = LocalServices
	manifest.Environment.Dependencies = map[string]string{"orka.kubernetes.policy": KubernetesPolicyVersion}
	for index, original := range manifest.Checks {
		manifest.Checks[index] = httpCheck(t)
		manifest.Checks[index].ID, manifest.Checks[index].Kind = original.ID, original.Kind
	}
	return manifest
}

func TestHTTPExpectationCanonicalEncoding(t *testing.T) {
	got := httpExpectation(t, 400, "a\n\"b\"")
	if got.ExitCode != 0 || got.Stdout != `{"status":400,"body":"a\n\"b\""}` || len(got.Services) != 0 {
		t.Fatalf("unexpected HTTP expectation: %#v", got)
	}
	for _, example := range []struct {
		status int
		body   string
	}{
		{199, "healthy"}, {600, "healthy"}, {200, string([]byte{0xff})},
		{200, strings.Repeat("x", MaxHTTPResponseBytes+1)},
		{200, strings.Repeat("\x00", MaxHTTPResponseBytes)},
	} {
		if _, err := HTTPExpectation(example.status, example.body); err == nil {
			t.Fatal("unsupported or oversized HTTP expectation was accepted")
		}
	}
	if _, err := HTTPExpectation(200, strings.Repeat("x", MaxHTTPResponseBytes)); err != nil {
		t.Fatal("exact response-size boundary was rejected")
	}
	overhead := len(httpExpectation(t, 200, "").Stdout)
	remaining := 8192 - overhead
	body := strings.Repeat("\x00", remaining/6) + strings.Repeat("a", remaining%6)
	if atLimit, err := HTTPExpectation(200, body); err != nil || len(atLimit.Stdout) != 8192 {
		t.Fatal("exact canonical JSON byte limit was rejected")
	}
	if _, err := HTTPExpectation(200, body+"a"); err == nil {
		t.Fatal("canonical JSON one byte beyond the limit was accepted")
	}
}

func TestHTTPCheckRejectsUnsupportedContracts(t *testing.T) {
	for name, mutate := range map[string]func(*Check){
		"missing contract":     func(check *Check) { check.HTTP = nil },
		"unknown version":      func(check *Check) { check.HTTP.Version++ },
		"raw oracle":           func(check *Check) { check.Command = []string{"/checks/linked-checker"} },
		"stdin":                func(check *Check) { check.Stdin = "in-process input" },
		"lifecycle":            func(check *Check) { check.Lifecycle = []string{"reconcile"} },
		"no server":            func(check *Check) { check.HTTP.ServerCommand = nil },
		"unfrozen server":      func(check *Check) { check.HTTP.ServerCommand[0] = "/src/server" },
		"server traversal":     func(check *Check) { check.HTTP.ServerCommand[0] = "/checks/../server" },
		"invalid argument":     func(check *Check) { check.HTTP.ServerCommand = append(check.HTTP.ServerCommand, "\x00") },
		"absolute URL":         func(check *Check) { check.HTTP.Path = "http://example.invalid/" },
		"authority":            func(check *Check) { check.HTTP.Path = "//example.invalid/" },
		"fragment":             func(check *Check) { check.HTTP.Path = "/quantity#ignored" },
		"header injection":     func(check *Check) { check.HTTP.Path = "/\r\nHost: example.invalid" },
		"empty path":           func(check *Check) { check.HTTP.Path = "" },
		"large path":           func(check *Check) { check.HTTP.Path = "/" + strings.Repeat("a", maxHTTPPathBytes) },
		"no deadline":          func(check *Check) { check.TimeoutSeconds = 0 },
		"large deadline":       func(check *Check) { check.TimeoutSeconds = 301 },
		"raw healthy output":   func(check *Check) { check.Healthy.Stdout = "healthy\n" },
		"child exit":           func(check *Check) { check.Healthy.ExitCode = 1 },
		"fixture expectations": func(check *Check) { check.Healthy.Services = map[string]string{"server": "healthy"} },
		"same expectations":    func(check *Check) { check.Failure = check.Healthy },
		"duplicate field":      func(check *Check) { check.Healthy.Stdout = `{"status":500,"status":200,"body":"healthy"}` },
		"unknown field":        func(check *Check) { check.Healthy.Stdout = `{"status":200,"body":"healthy","httpCompleted":true}` },
		"extra document":       func(check *Check) { check.Healthy.Stdout += "{}" },
		"noncanonical JSON":    func(check *Check) { check.Healthy.Stdout = `{"body":"healthy","status":200}` },
	} {
		t.Run(name, func(t *testing.T) {
			check := httpCheck(t)
			mutate(&check)
			if err := ValidateHTTPCheck(check); err == nil {
				t.Fatal("unsupported protected HTTP contract was accepted")
			}
		})
	}
}

func TestHTTPManifestCompatibilityGate(t *testing.T) {
	if err := ValidateManifest(httpManifest(t)); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Manifest){
		"offline": func(manifest *Manifest) { manifest.Environment.Profile = Offline },
		"old worker": func(manifest *Manifest) {
			manifest.Environment.Dependencies["orka.kubernetes.policy"] = "kubernetes-v2-process-seccomp"
		},
		"Docker": func(manifest *Manifest) {
			manifest.Environment.Dependencies["orka.local-runner.policy"] = "unsupported-local-executor"
		},
		"raw mixed checks": func(manifest *Manifest) { manifest.Checks[0] = testManifest().Checks[0] },
		"additional fixture": func(manifest *Manifest) {
			manifest.Environment.Services = []Service{{ID: "extra", Port: 9000, Command: []string{"/checks/run"}, ReadyOutput: "ready"}}
		},
		"outbound": func(manifest *Manifest) {
			manifest.Environment.Requirements = []EnvironmentRequirement{{Kind: "external-service", Name: "remote"}}
		},
		"cluster": func(manifest *Manifest) {
			manifest.Environment.Requirements = []EnvironmentRequirement{{Kind: "cluster", Name: "full-cluster"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			manifest := httpManifest(t)
			mutate(&manifest)
			if err := ValidateManifest(manifest); err == nil {
				t.Fatal("unsupported protected HTTP environment was accepted")
			}
		})
	}
	legacy := testManifest()
	legacy.Environment.Dependencies = map[string]string{"orka.kubernetes.policy": "kubernetes-v2-process-seccomp"}
	binding, observations := testEvidence(legacy)
	if got := Evaluate(legacy, binding, observations); got.Conclusion != Verified {
		t.Fatal("historical command-only evidence is no longer readable")
	}
}

func TestHTTPEvaluationRequiresObserverCompletion(t *testing.T) {
	for _, action := range []Action{"", ValidateReport, VerifyPatch} {
		t.Run(string(action), func(t *testing.T) {
			manifest := httpManifest(t)
			manifest.Action = action
			if action != "" {
				var err error
				manifest.ReportDigest, err = StableReportDigest(manifest.Problem, manifest.Scope)
				if err != nil {
					t.Fatal(err)
				}
			}
			if action == ValidateReport {
				manifest.Sources.Patched = SourceIdentity{}
				manifest.Sources.DiffDigest = ""
			}
			if action == VerifyPatch {
				manifest.DeclaredChanges = []DeclaredChange{{Kind: "source", Description: "synthetic response fix"}}
			}
			binding, observations := testEvidence(manifest)
			expected, unavailable := Verified, UnableToVerify
			if action == ValidateReport {
				binding.PatchedTaskID = ""
				observations = observations[:len(manifest.Checks)]
				expected, unavailable = Reproduced, UnableToValidate
			}
			for index := range observations {
				observations[index].HTTPCompleted = true
			}
			if got := Evaluate(manifest, binding, observations); got.Conclusion != expected {
				t.Fatalf("completed observations yielded %s: %s", got.Conclusion, got.Reason)
			}
			for index := range observations {
				observations[index].HTTPCompleted = false
				if got := Evaluate(manifest, binding, observations); got.Conclusion != unavailable {
					t.Fatalf("matching output without HTTP completion yielded %s", got.Conclusion)
				}
				observations[index].HTTPCompleted = true
			}
			observations[0].SetupError = "subject died before orderly shutdown"
			if got := Evaluate(manifest, binding, observations); got.Conclusion != unavailable {
				t.Fatal("HTTP completion overrode an unusable lifecycle")
			}
		})
	}
	legacy := testManifest()
	binding, observations := testEvidence(legacy)
	observations[0].HTTPCompleted = true
	if got := Evaluate(legacy, binding, observations); got.Conclusion != UnableToVerify {
		t.Fatal("a raw command claimed HTTP observer authority")
	}
}

func httpFixture(t *testing.T, handler http.HandlerFunc) int {
	t.Helper()
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	if err := server.Listener.Close(); err != nil {
		t.Fatal(err)
	}
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	return listener.Addr().(*net.TCPAddr).Port
}

func TestObserveHTTPActualResponse(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	requests := make(chan *http.Request, 1)
	port := httpFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		requests <- request
		writer.WriteHeader(400)
		_, _ = writer.Write([]byte("rejected\n"))
	})
	check := httpCheck(t)
	content, err := ObserveHTTP(context.Background(), check, port)
	expected := httpExpectation(t, 400, "rejected\n")
	if err != nil || string(content) != expected.Stdout {
		t.Fatalf("HTTP response was not observed exactly: %v, %q", err, content)
	}
	request := <-requests
	if request.Method != http.MethodGet || request.RequestURI != check.HTTP.Path ||
		request.Header.Get("Accept-Encoding") != "" || request.URL.IsAbs() {
		t.Fatal("observer did not use the fixed plain GET contract")
	}
}

func TestObserveHTTPDoesNotFollowRedirects(t *testing.T) {
	var calls atomic.Int32
	port := httpFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		writer.Header().Set("Location", "/would-hide-the-broken-response")
		writer.WriteHeader(http.StatusFound)
		_, _ = writer.Write([]byte("redirected"))
	})
	content, err := ObserveHTTP(context.Background(), httpCheck(t), port)
	if err != nil || string(content) != httpExpectation(t, http.StatusFound, "redirected").Stdout || calls.Load() != 1 {
		t.Fatalf("observer followed a redirect or lost the actual response: %v", err)
	}
}

func TestObserveHTTPResponseByteLimit(t *testing.T) {
	for _, size := range []int{MaxHTTPResponseBytes - 1, MaxHTTPResponseBytes, MaxHTTPResponseBytes + 1} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			body := strings.Repeat("x", size)
			port := httpFixture(t, func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = writer.Write([]byte(body))
			})
			content, err := ObserveHTTP(context.Background(), httpCheck(t), port)
			if size > MaxHTTPResponseBytes {
				if err == nil || len(content) != 0 {
					t.Fatal("response exceeded the exact body-byte limit")
				}
				return
			}
			if err != nil || string(content) != httpExpectation(t, 200, body).Stdout {
				t.Fatalf("bounded response was not preserved: %v", err)
			}
		})
	}
}

func TestObserveHTTPIncompleteResponses(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"oversized body": func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write([]byte(strings.Repeat("x", MaxHTTPResponseBytes+1)))
		},
		"invalid UTF-8": func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write([]byte{0xff})
		},
		"truncated body": func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Length", "200")
			_, _ = writer.Write([]byte("healthy"))
		},
		"oversized headers": func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("X-Oversized", strings.Repeat("x", 9000))
			_, _ = writer.Write([]byte("healthy"))
		},
		"late body": func(writer http.ResponseWriter, request *http.Request) {
			writer.WriteHeader(200)
			writer.(http.Flusher).Flush()
			<-request.Context().Done()
			_, _ = writer.Write([]byte("healthy"))
		},
		"late headers": func(writer http.ResponseWriter, request *http.Request) {
			<-request.Context().Done()
			_, _ = writer.Write([]byte("healthy"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			port := httpFixture(t, handler)
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			content, err := ObserveHTTP(ctx, httpCheck(t), port)
			if err == nil || len(content) != 0 {
				t.Fatal("late, truncated, or oversized response produced usable observation bytes")
			}
		})
	}
	t.Run("canceled before request", func(t *testing.T) {
		var calls atomic.Int32
		port := httpFixture(t, func(http.ResponseWriter, *http.Request) { calls.Add(1) })
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		content, err := ObserveHTTP(ctx, httpCheck(t), port)
		if err == nil || len(content) != 0 || calls.Load() != 0 {
			t.Fatal("canceled HTTP observation contacted the subject or produced output")
		}
	})
}

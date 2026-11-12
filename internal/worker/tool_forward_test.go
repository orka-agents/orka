package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strconv"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/outboundaccess"
)

func forwardingTestTool(url string) *corev1alpha1.Tool {
	return &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "azure-arm", Namespace: "tenant"},
		Spec: corev1alpha1.ToolSpec{
			Description:       "Azure Resource Manager",
			BrokeredToolClass: corev1alpha1.AgentRuntimeBrokeredToolClassRead,
			HTTP: &corev1alpha1.HTTPExecution{
				URL:                     url,
				Method:                  http.MethodPost,
				OutboundAccessPolicyRef: &corev1alpha1.LocalObjectReference{Name: "azure"},
				Forward:                 &corev1alpha1.HTTPForward{Protocol: corev1alpha1.HTTPForwardProtocolAzureResourceManager},
			},
		},
	}
}

func agentIdentityResolver() *fakeOutboundAccessResolver {
	return &fakeOutboundAccessResolver{resolution: outboundaccess.Resolution{
		Adapter: outboundaccess.AdapterAgentIdentity, CredentialHeader: "Authorization",
		CredentialValue: "Bearer arm-token-secret", SensitiveValues: []string{"arm-token-secret"},
	}}
}

func TestForwardingToolSendsTheReadWithTheCredentialAndReturnsAnyStatus(t *testing.T) {
	var got *http.Request
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "session=leak")
		w.Header().Set("X-Ms-Request-Id", "req-1")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"ResourceGroupNotFound","message":"echo arm-token-secret"}}`))
	}))
	defer server.Close()
	resolver := agentIdentityResolver()
	executor := &ToolExecutor{client: server.Client(), namespace: "tenant", outboundResolver: resolver, skipDirectPublicValidation: true}
	executor.AllowForwarding()

	result, err := executor.Execute(context.Background(), forwardingTestTool(server.URL),
		json.RawMessage(`{"method":"GET","path":"/subscriptions/s1/resourcegroups/rg1","query":"api-version=2024-11-01"}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got == nil || got.Method != http.MethodGet || got.URL.Path != "/subscriptions/s1/resourcegroups/rg1" ||
		got.URL.RawQuery != "api-version=2024-11-01" || got.Header.Get("Authorization") != "Bearer arm-token-secret" {
		t.Fatalf("upstream request = %#v", got)
	}
	if resolver.request.Tool.Forward != corev1alpha1.HTTPForwardProtocolAzureResourceManager || resolver.request.Tool.Method != http.MethodGet {
		t.Fatalf("resolver tool binding = %#v", resolver.request.Tool)
	}
	response, err := harnessv2.DecodeMCPForwardResponse(json.RawMessage(result))
	if err != nil {
		t.Fatalf("result %q: %v", result, err)
	}
	if response.Status != http.StatusNotFound || response.Headers["X-Ms-Request-Id"] != "req-1" || response.Headers["Set-Cookie"] != "" {
		t.Fatalf("response = %#v", response)
	}
	if strings.Contains(response.Body, "arm-token-secret") || !strings.Contains(response.Body, "ResourceGroupNotFound") {
		t.Fatalf("body = %q", response.Body)
	}
}

func TestForwardingToolDoesNotFollowRedirects(t *testing.T) {
	leaked := false
	sink := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("Authorization") != ""
	}))
	defer sink.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL+"/collect", http.StatusFound)
	}))
	defer server.Close()
	executor := &ToolExecutor{client: server.Client(), namespace: "tenant", outboundResolver: agentIdentityResolver(), skipDirectPublicValidation: true}
	executor.AllowForwarding()
	result, err := executor.Execute(context.Background(), forwardingTestTool(server.URL), json.RawMessage(`{"method":"GET","path":"/subscriptions"}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	response, err := harnessv2.DecodeMCPForwardResponse(json.RawMessage(result))
	if err != nil || response.Status != http.StatusFound || leaked {
		t.Fatalf("response = %#v, err = %v, leaked = %v", response, err, leaked)
	}
}

func TestForwardingToolRefusals(t *testing.T) {
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	for _, tc := range []struct {
		name      string
		forward   bool
		resolver  *fakeOutboundAccessResolver
		arguments string
		want      string
	}{
		{"outside the ACP broker", false, agentIdentityResolver(), `{"method":"GET","path":"/subscriptions"}`, "runs only through an ACP runtime"},
		{"write", true, agentIdentityResolver(), `{"method":"PUT","path":"/subscriptions/s1/resourcegroups/rg"}`, "only GET and HEAD"},
		{"dot segment", true, agentIdentityResolver(), `{"method":"GET","path":"/subscriptions/../tenants"}`, "dot segments"},
		{"another origin", true, agentIdentityResolver(), `{"method":"GET","path":"//evil.example/x"}`, "absolute"},
		{"extra argument", true, agentIdentityResolver(), `{"method":"GET","path":"/subscriptions","body":"x"}`, "forwarding arguments"},
		{"gateway policy", true, &fakeOutboundAccessResolver{resolution: outboundaccess.Resolution{Adapter: outboundaccess.AdapterGateway}},
			`{"method":"GET","path":"/subscriptions"}`, "not a gateway"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executor := &ToolExecutor{client: server.Client(), namespace: "tenant", outboundResolver: tc.resolver, skipDirectPublicValidation: true}
			if tc.forward {
				executor.AllowForwarding()
			}
			_, err := executor.Execute(context.Background(), forwardingTestTool(server.URL), json.RawMessage(tc.arguments))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Execute() error = %v, want %q", err, tc.want)
			}
		})
	}
	if requests != 0 {
		t.Fatalf("refused calls sent %d requests", requests)
	}
}

func TestForwardingToolRefusesAResultTooLargeOnceEncoded(t *testing.T) {
	// "<" encodes as six bytes, so a body under the raw limit overflows the
	// broker's result bound after JSON escaping.
	body := strings.Repeat("<", 600<<10)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	executor := &ToolExecutor{client: server.Client(), namespace: "tenant", outboundResolver: agentIdentityResolver(), skipDirectPublicValidation: true}
	executor.AllowForwarding()
	_, err := executor.Execute(context.Background(), forwardingTestTool(server.URL), json.RawMessage(`{"method":"GET","path":"/subscriptions"}`))
	if _, ok := errors.AsType[ToolExecutionError](err); !ok || !strings.Contains(err.Error(), "once encoded") {
		t.Fatalf("Execute() error = %v, want a tool execution error", err)
	}
}

func TestForwardingToolCarriesTheLengthOfAHeadResponse(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte("body"))
		}
	}))
	defer server.Close()
	executor := &ToolExecutor{client: server.Client(), namespace: "tenant", outboundResolver: agentIdentityResolver(), skipDirectPublicValidation: true}
	executor.AllowForwarding()
	for method, want := range map[string]string{http.MethodHead: "4", http.MethodGet: ""} {
		result, err := executor.Execute(context.Background(), forwardingTestTool(server.URL), json.RawMessage(`{"method":"`+method+`","path":"/subscriptions"}`))
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		response, err := harnessv2.DecodeMCPForwardResponse(json.RawMessage(result))
		if err != nil || response.Headers["Content-Length"] != want {
			t.Fatalf("%s: response = %#v, err = %v", method, response, err)
		}
	}
}

// TestForwardingToolKeepsContinuationTokens covers ARM paging: a nextLink's
// $skiptoken, in the body or a URL-valued header, survives redaction so the
// next page can be requested, while the injected credential and other
// secret-shaped values are still removed.
func TestForwardingToolKeepsContinuationTokens(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Location", "https://management.azure.com/subscriptions?api-version=2022-12-01&%24skiptoken=hdr-page2")
		_, _ = w.Write([]byte(`{"value":[{"id":"/subscriptions/s1","tags":{"note":"password=hunter2 arm-token-secret"}}],` +
			`"nextLink":"https://management.azure.com/subscriptions?api-version=2022-12-01&$skiptoken=page2"}`))
	}))
	defer server.Close()
	executor := &ToolExecutor{client: server.Client(), namespace: "tenant", outboundResolver: agentIdentityResolver(), skipDirectPublicValidation: true}
	executor.AllowForwarding()
	result, err := executor.Execute(context.Background(), forwardingTestTool(server.URL),
		json.RawMessage(`{"method":"GET","path":"/subscriptions","query":"api-version=2022-12-01"}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	response, err := harnessv2.DecodeMCPForwardResponse(json.RawMessage(result))
	if err != nil {
		t.Fatalf("result %q: %v", result, err)
	}
	if !strings.Contains(response.Body, `&$skiptoken=page2"`) {
		t.Fatalf("body lost the continuation token: %q", response.Body)
	}
	if location := response.Headers["Location"]; !strings.HasSuffix(location, "&%24skiptoken=hdr-page2") {
		t.Fatalf("Location lost the continuation token: %q", location)
	}
	if strings.Contains(response.Body, "hunter2") || strings.Contains(response.Body, "arm-token-secret") {
		t.Fatalf("body kept a secret: %q", response.Body)
	}
	if strings.Contains(response.Body, "\x00") || strings.Contains(response.Body, "orka-forward-continuation") {
		t.Fatalf("body kept a placeholder: %q", response.Body)
	}
}

func TestRedactForwardedTextRemovesAnInjectedCredentialFromAContinuationToken(t *testing.T) {
	got := redactForwardedText(`{"nextLink":"https://x/?$skiptoken=arm-token-secret"}`, "arm-token-secret")
	if strings.Contains(got, "arm-token-secret") {
		t.Fatalf("injected credential survived in a continuation token: %q", got)
	}
}

func TestRedactForwardedTextKeepsJSONEscapedContinuationTokens(t *testing.T) {
	for _, body := range []string{
		`{"nextLink":"https://management.azure.com/subscriptions?api-version=1\u0026$skiptoken=page2"}`,
		`{"nextLink":"https://management.azure.com/subscriptions?api-version=1\u0026\u0024skiptoken=page2\u0026x=1"}`,
	} {
		got := redactForwardedText(body)
		if !strings.Contains(got, "skiptoken=page2") || strings.Contains(got, "[REDACTED]") {
			t.Fatalf("escaped continuation token lost: %s -> %s", body, got)
		}
		if got != body {
			t.Fatalf("the body changed: %s -> %s", body, got)
		}
	}
}

func TestRedactForwardedBodyKeepsJSONValid(t *testing.T) {
	body := `{"secrets":[{"name":"example"}],"secret":null,"count":3,` +
		`"properties":{"password":"hunter2","note":"token=abc rest","accessToken":"tok-1","items":[1,true,null]},` +
		`"nextLink":"https://management.azure.com/x?api-version=1\u0026$skiptoken=page2","$skipToken":"page3",` +
		`"echo":"arm-token-secret"}`
	got := redactForwardedBody(body, "arm-token-secret")
	if !json.Valid([]byte(got)) {
		t.Fatalf("redacted body is not JSON: %s", got)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatal(err)
	}
	properties := decoded["properties"].(map[string]any)
	switch {
	case decoded["secret"] != nil:
		t.Fatalf("a null under a sensitive key changed: %v", decoded["secret"])
	case decoded["secrets"].([]any)[0].(map[string]any)["name"] != "example":
		t.Fatalf("an array under a sensitive key changed: %v", decoded["secrets"])
	case properties["password"] != "[REDACTED]" || properties["accessToken"] != "[REDACTED]":
		t.Fatalf("sensitive string values kept: %v", properties)
	case strings.Contains(properties["note"].(string), "abc"):
		t.Fatalf("a secret-shaped string kept its value: %q", properties["note"])
	case !strings.HasSuffix(decoded["nextLink"].(string), "&$skiptoken=page2") || decoded["$skipToken"] != "page3":
		t.Fatalf("paging state changed: %v %v", decoded["nextLink"], decoded["$skipToken"])
	case strings.Contains(got, "arm-token-secret"):
		t.Fatalf("the injected credential survived: %s", got)
	case decoded["count"] != float64(3) || len(properties["items"].([]any)) != 3:
		t.Fatalf("non-string values changed: %s", got)
	}
	if text := redactForwardedBody("not json token=abc", "x"); strings.Contains(text, "abc") {
		t.Fatalf("a text body was not redacted: %q", text)
	}
}

func TestRedactForwardedBodyRemovesAnEscapedInjectedCredential(t *testing.T) {
	const secret = "opaque-example-value"
	for _, body := range []string{
		`{"echo":"opaque\u002dexample-value"}`,
		`{"opaque\u002dexample-value":"key"}`,
		`{"nextLink":"https://x/?a=1\u0026$skiptoken=opaque\u002dexample-value"}`,
	} {
		got := redactForwardedBody(body, secret)
		var decoded any
		if err := json.Unmarshal([]byte(got), &decoded); err != nil {
			t.Fatalf("redacted body is not JSON: %s", got)
		}
		text, err := json.Marshal(decoded)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(text), secret) {
			t.Fatalf("decoded %s still holds the credential: %s", body, text)
		}
	}
}

func TestRedactForwardedTextRemovesAPercentEncodedCredential(t *testing.T) {
	const secret = "opaque-example-value"
	for _, text := range []string{
		`https://management.azure.com/x?api-version=1&$skiptoken=opaque%2Dexample%2Dvalue`,
		`https://management.azure.com/x?api-version=1&%24skiptoken=opaque%2dexample-value&a=1`,
	} {
		got := redactForwardedText(text, secret)
		decoded, err := neturl.QueryUnescape(got)
		if err != nil {
			decoded = got
		}
		if strings.Contains(decoded, secret) {
			t.Fatalf("decoded %s still holds the credential: %s", text, decoded)
		}
	}
	page := `https://management.azure.com/x?api-version=1&$skiptoken=page%2Dtwo`
	if got := redactForwardedText(page, secret); got != page {
		t.Fatalf("an ordinary paging token changed: %s -> %s", page, got)
	}
	body := `{"nextLink":"https://management.azure.com/x?a=1&$skiptoken=opaque%2Dexample%2Dvalue"}`
	if got := redactForwardedBody(body, secret); strings.Contains(got, "opaque%2Dexample%2Dvalue") {
		t.Fatalf("a JSON nextLink kept the encoded credential: %s", got)
	}
}

// TestRedactForwardedTextRemovesACredentialInAnyURLPart covers an upstream
// echoing the credential with non-standard escapes in an ordinary query value
// or a path segment, as in a Location header or a JSON URL: no decoding
// recovers it, and the bytes around it are kept.
func TestRedactForwardedTextRemovesACredentialInAnyURLPart(t *testing.T) {
	const secret = "opaque-example-value"
	for text, want := range map[string]string{
		`https://management.azure.com/x?echo=opaque%2Dexample%2Dvalue&api-version=1`:    `https://management.azure.com/x?echo=[REDACTED]&api-version=1`,
		`https://management.azure.com/opaque%2dexample%2dvalue/providers?api-version=1`: `https://management.azure.com/[REDACTED]/providers?api-version=1`,
		`https://management.azure.com/x?echo=%6Fpaque-example-valu%65s`:                 `https://management.azure.com/x?echo=[REDACTED]s`,
	} {
		if got := redactForwardedText(text, secret); got != want {
			t.Fatalf("redactForwardedText(%s) = %s, want %s", text, got, want)
		}
	}
	body := `{"id":"/subscriptions/opaque%2Dexample%2Dvalue/x","other":"100%25 kept"}`
	got := redactForwardedBody(body, secret)
	if want := `{"id":"/subscriptions/[REDACTED]/x","other":"100%25 kept"}`; got != want {
		t.Fatalf("redactForwardedBody() = %s, want %s", got, want)
	}
	header := http.Header{"Location": {"https://management.azure.com/x?echo=opaque%2Dexample%2Dvalue"}}
	if got := redactForwardedText(header.Get("Location"), secret); strings.Contains(got, "opaque") {
		t.Fatalf("a Location header kept the encoded credential: %s", got)
	}
}

// TestRedactForwardedBodyKeepsJSONValidForAnyCredentialPlacement covers a
// credential the upstream echoes as a number, a string, a key, or nested:
// the redacted body is always valid JSON, so a success status never carries
// a body the runtime cannot decode.
func TestRedactForwardedBodyKeepsJSONValidForAnyCredentialPlacement(t *testing.T) {
	const secret = "123456789"
	for _, body := range []string{
		`{"pin":123456789,"ok":true}`,
		`{"pin": 123456789 ,"n":[1, 123456789]}`,
		`{"echo":"x123456789y","123456789":null}`,
		`[123456789, {"a": "123456789"}]`,
	} {
		got := redactForwardedBody(body, secret)
		if !json.Valid([]byte(got)) {
			t.Fatalf("redacted body is not JSON: %s -> %s", body, got)
		}
		if strings.Contains(got, secret) {
			t.Fatalf("redacted body still holds the credential: %s -> %s", body, got)
		}
	}
	if got := redactForwardedBody(`{"n":42}`, secret); got != `{"n":42}` {
		t.Fatalf("an unrelated number changed: %s", got)
	}
}

// TestRedactForwardedTextRestoresManyContinuationTokens covers a response
// with thousands of paging links: every kept continuation parameter comes
// back in place, in one pass over the text.
func TestRedactForwardedTextRestoresManyContinuationTokens(t *testing.T) {
	var text strings.Builder
	for i := range 5000 {
		text.WriteString("https://management.azure.com/x?api-version=1&$skiptoken=page-" + strconv.Itoa(i) + "\n")
	}
	if got := redactForwardedText(text.String(), "opaque-example-value"); got != text.String() {
		t.Fatal("continuation parameters were not all restored in place")
	}
}

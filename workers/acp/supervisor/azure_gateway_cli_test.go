package supervisor

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// TestAzureGatewayServesTheRealAzureCLI runs a stock Azure CLI against the
// gateway, with a broker that answers like Resource Manager. Orka signs az in,
// so no command runs `az login`. It needs az, so it runs only when
// ORKA_TEST_AZ_CLI names the binary.
func TestAzureGatewayServesTheRealAzureCLI(t *testing.T) {
	az := os.Getenv("ORKA_TEST_AZ_CLI")
	if az == "" {
		t.Skip("set ORKA_TEST_AZ_CLI to an az binary to run")
	}
	fixture := newAzureGatewayFixtureSigningIn(t, false, func(request harnessv2.MCPBrokerCallRequest) json.RawMessage {
		forwarded, err := harnessv2.DecodeMCPForwardRequest(request.Call.Arguments)
		if err != nil {
			t.Errorf("forwarded arguments: %v", err)
		}
		response := harnessv2.MCPForwardResponse{Status: http.StatusOK, Headers: map[string]string{"Content-Type": "application/json"}}
		switch {
		case forwarded.Path == "/subscriptions":
			response.Body = `{"value":[{"id":"/subscriptions/11111111-1111-1111-1111-111111111111","subscriptionId":"11111111-1111-1111-1111-111111111111","tenantId":"22222222-2222-2222-2222-222222222222","displayName":"orka-test","state":"Enabled"}]}`
		case strings.HasSuffix(strings.ToLower(forwarded.Path), "/resourcegroups") && !strings.Contains(forwarded.Query, "skiptoken"):
			response.Body = `{"value":[{"id":"/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg-one","name":"rg-one","location":"eastus","properties":{"provisioningState":"Succeeded"}}],` +
				`"nextLink":"https://management.azure.com/subscriptions/11111111-1111-1111-1111-111111111111/resourcegroups?api-version=2024-11-01&$skiptoken=page2"}`
		case strings.HasSuffix(strings.ToLower(forwarded.Path), "/resourcegroups"):
			response.Body = `{"value":[{"id":"/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/rg-two","name":"rg-two","location":"westus","properties":{"provisioningState":"Succeeded"}}]}`
		default:
			response.Status = http.StatusNotFound
			response.Body = `{"error":{"code":"ResourceNotFound","message":"not in the fixture"}}`
		}
		raw, _ := json.Marshal(response)
		return raw
	})
	fixture.waitForSignIn(t)
	run := azRunner(az, fixture)
	output, err := run("group", "list", "--query", "[].name", "--output", "tsv")
	if fields := strings.Fields(output); err != nil || len(fields) == 0 || fields[0] != "rg-one" || !strings.Contains(output, "rg-two") {
		t.Fatalf("az group list (with a second page): %v\n%s", err, output)
	}
	output, err = run("account", "show", "--query", "{tenant:tenantId,subscription:id}", "--output", "json")
	if err != nil || !strings.Contains(output, "11111111-1111-1111-1111-111111111111") {
		t.Fatalf("az account show: %v\n%s", err, output)
	}
	t.Logf("az account show: %s", strings.TrimSpace(output))
	output, err = run("group", "create", "--name", "rg-three", "--location", "eastus")
	if err == nil || !strings.Contains(output, "OrkaReadOnly") {
		t.Fatalf("az group create should be refused as a write: %v\n%s", err, output)
	}
	checkAzNamesTheMissingLink(t, fixture, run)
	// After az forgets its sign-in, az's own managed-identity login works.
	if output, err := run("account", "clear"); err != nil {
		t.Fatalf("az account clear: %v\n%s", err, output)
	}
	if output, err := run("login", "--identity", "--output", "none"); err != nil {
		t.Fatalf("az login --identity: %v\n%s", err, output)
	}
	if output, err := run("group", "list", "--query", "[].name", "--output", "tsv"); err != nil || !strings.Contains(output, "rg-one") {
		t.Fatalf("az group list after az login --identity: %v\n%s", err, output)
	}
	for _, call := range fixture.brokerCalls() {
		if strings.Contains(string(call.Call.Arguments), "rg-three") {
			t.Fatal("the refused write reached the broker")
		}
	}
}

// TestAzureGatewayPlaceholderAccountWorksInTheRealAzureCLI checks that az
// accepts the placeholder profile and shows why the sign-in failed, not
// "Please run 'az login'".
func TestAzureGatewayPlaceholderAccountWorksInTheRealAzureCLI(t *testing.T) {
	az := os.Getenv("ORKA_TEST_AZ_CLI")
	if az == "" {
		t.Skip("set ORKA_TEST_AZ_CLI to an az binary to run")
	}
	fixture := newAzureGatewayFixtureSigningIn(t, false, func(harnessv2.MCPBrokerCallRequest) json.RawMessage {
		return json.RawMessage(`{"isError":true,"code":"connection_required","error":"Azure is not linked for the person who asked. They need to run ` +
			"`orka connect azure -n team-a`" + `, then run this Task again."}`)
	})
	waitForAttempt(t, fixture)
	run := azRunner(az, fixture)
	// An agent that looks before the sign-in finishes reads that it should
	// retry, not that the identity has no subscription.
	if output, err := run("account", "show", "--query", "name", "-o", "tsv"); err != nil || strings.TrimSpace(output) != azureSignInPendingName {
		t.Fatalf("az account show under the placeholder: %v\n%s", err, output)
	}
	output, err := run("group", "list")
	if err == nil || !strings.Contains(output, "OrkaConnectionRequired") || !strings.Contains(output, "orka connect azure -n team-a") ||
		strings.Contains(output, "Please run 'az login'") {
		t.Fatalf("az group list under the placeholder should name the fix: %v\n%s", err, output)
	}
	// Once the account is linked, the next command signs in and asks to be
	// run again, and the one after that works.
	fixture.mu.Lock()
	fixture.respond = func(request harnessv2.MCPBrokerCallRequest) json.RawMessage {
		forwarded, _ := harnessv2.DecodeMCPForwardRequest(request.Call.Arguments)
		response := harnessv2.MCPForwardResponse{Status: http.StatusOK, Headers: map[string]string{"Content-Type": "application/json"}, Body: `{"value":[]}`}
		if forwarded.Path == "/subscriptions" {
			response.Body = `{"value":[{"subscriptionId":"11111111-1111-1111-1111-111111111111","tenantId":"22222222-2222-2222-2222-222222222222","displayName":"orka-test","state":"Enabled"}]}`
		}
		raw, _ := json.Marshal(response)
		return raw
	}
	fixture.mu.Unlock()
	output, err = run("group", "list")
	if err == nil || !strings.Contains(output, "OrkaSignedIn") {
		t.Fatalf("az group list should ask to be run again: %v\n%s", err, output)
	}
	if output, err := run("group", "list"); err != nil {
		t.Fatalf("az group list after the sign-in: %v\n%s", err, output)
	}
}

func azRunner(az string, fixture *azureGatewayFixture) func(...string) (string, error) {
	return func(args ...string) (string, error) {
		command := exec.Command(az, args...)
		command.Env = []string{"HOME=" + fixture.home, "PATH=" + os.Getenv("PATH")}
		for name, value := range fixture.env {
			command.Env = append(command.Env, name+"="+value)
		}
		output, err := command.CombinedOutput()
		return string(output), err
	}
}

// checkAzNamesTheMissingLink checks that with no usable linked account the
// error names the fix.
func checkAzNamesTheMissingLink(t *testing.T, fixture *azureGatewayFixture, run func(...string) (string, error)) {
	t.Helper()
	fixture.mu.Lock()
	linked := fixture.respond
	fixture.respond = func(harnessv2.MCPBrokerCallRequest) json.RawMessage {
		return json.RawMessage(`{"isError":true,"code":"connection_required","error":"Azure is not linked for the person who asked. They need to run ` +
			"`orka connect azure -n team-a`" + `, then run this Task again."}`)
	}
	fixture.mu.Unlock()
	output, err := run("group", "list")
	if err == nil || !strings.Contains(output, "OrkaConnectionRequired") || !strings.Contains(output, "orka connect azure -n team-a") {
		t.Fatalf("az group list without a link should name the fix: %v\n%s", err, output)
	}
	fixture.mu.Lock()
	fixture.respond = linked
	fixture.mu.Unlock()
}

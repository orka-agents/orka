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

// TestAzureGatewayServesTheRealAzureCLI runs the Azure CLI against the
// gateway, with a broker that answers like Resource Manager. It needs az, so
// it runs only when ORKA_TEST_AZ_CLI names the binary; set
// ORKA_TEST_AZ_CLI_SIGNS_IN when that binary is the toolbox wrapper.
func TestAzureGatewayServesTheRealAzureCLI(t *testing.T) {
	az := os.Getenv("ORKA_TEST_AZ_CLI")
	if az == "" {
		t.Skip("set ORKA_TEST_AZ_CLI to an az binary to run")
	}
	fixture := newAzureGatewayFixture(t)
	fixture.respond = func(request harnessv2.MCPBrokerCallRequest) json.RawMessage {
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
	}
	run := func(args ...string) (string, error) {
		command := exec.Command(az, args...)
		command.Env = []string{"HOME=" + fixture.home, "PATH=" + os.Getenv("PATH")}
		for name, value := range fixture.env {
			command.Env = append(command.Env, name+"="+value)
		}
		output, err := command.CombinedOutput()
		return string(output), err
	}
	// The toolbox wrapper signs in on the first command; a plain az needs the
	// explicit login.
	if os.Getenv("ORKA_TEST_AZ_CLI_SIGNS_IN") == "" {
		if output, err := run("login", "--identity", "--output", "none"); err != nil {
			t.Fatalf("az login --identity: %v\n%s", err, output)
		}
	}
	output, err := run("group", "list", "--query", "[].name", "--output", "tsv")
	if err != nil || strings.Fields(output)[0] != "rg-one" || !strings.Contains(output, "rg-two") {
		t.Fatalf("az group list (with a second page): %v\n%s", err, output)
	}
	if os.Getenv("ORKA_TEST_AZ_CLI_SIGNS_IN") != "" {
		// The wrapper signs in again after the login is cleared.
		if output, err := run("account", "clear"); err != nil {
			t.Fatalf("az account clear: %v\n%s", err, output)
		}
		if output, err := run("group", "list", "--query", "[].name", "--output", "tsv"); err != nil || !strings.Contains(output, "rg-one") {
			t.Fatalf("az group list after account clear: %v\n%s", err, output)
		}
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
	if os.Getenv("ORKA_TEST_AZ_CLI_SIGNS_IN") != "" {
		// Outside a running prompt the wrapper's sign-in fails, and it says why.
		fixture.session.deactivate(fixture.authorize.PromptID, harnessv2.RuntimeSessionStateIdle)
		if output, err := run("account", "clear"); err != nil {
			t.Fatalf("az account clear: %v\n%s", err, output)
		}
		output, err := run("group", "list")
		if err == nil || !strings.Contains(output, "Orka could not sign az in") || !strings.Contains(output, "OrkaNotAuthorized") {
			t.Fatalf("az group list outside a prompt should explain the refusal: %v\n%s", err, output)
		}
	}
	for _, call := range fixture.brokerCalls() {
		if strings.Contains(string(call.Call.Arguments), "rg-three") {
			t.Fatal("the refused write reached the broker")
		}
	}
}

package controller

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/store"
)

// repositoryMonitorEvidenceLeakCase is CI text that carries a credential in
// an ordinary shape: a shell trace, a config dump, or a diff. None of
// secrets may reach the repair prompt; keep lists ordinary text around it
// that must survive.
type repositoryMonitorEvidenceLeakCase struct {
	name    string
	input   string
	secrets []string
	keep    []string
}

// Synthetic credential values, assembled at run time so the source holds no
// literal credential.
var (
	repositoryMonitorEvidenceLeakValue  = "Vq7Rk2Lm" + "9Xw4Tz8Np3Hs"
	repositoryMonitorEvidenceLeakBase64 = "ZGVwbG95" + "OnMzY3JldFZhbHVlOTk="
	repositoryMonitorEvidenceLeakBody   = "Ab3Cd5Ef" + strings.Repeat("7Gh9Jk2Lm4", 4)
)

func repositoryMonitorEvidenceLeakCases() []repositoryMonitorEvidenceLeakCase {
	v, b, body := repositoryMonitorEvidenceLeakValue, repositoryMonitorEvidenceLeakBase64, repositoryMonitorEvidenceLeakBody
	prefixes := []string{"ya29.", "sk_" + "live_", "sk_" + "test_", "rk_" + "live_", "AI" + "za", "gl" + "pat-", "npm" + "_", "hv" + "s.", "dckr" + "_pat_", "xo" + "xb-", "xo" + "xp-", "xo" + "xa-", "xo" + "xr-"}
	prefixCases := make([]repositoryMonitorEvidenceLeakCase, 0, len(prefixes))
	for _, prefix := range prefixes {
		prefixCases = append(prefixCases, repositoryMonitorEvidenceLeakCase{name: "token prefix " + prefix, input: "deploy failed with " + prefix + body + " (401)", secrets: []string{body}, keep: []string{"(401)"}})
	}
	cases := slices.Concat([]repositoryMonitorEvidenceLeakCase{
		{name: "docker login --password", input: "+ docker login registry.example.invalid -u ci --password " + v},
		{name: "docker login -p", input: "+ docker login -p " + "dckr" + "_pat_" + body + " registry.example.invalid", secrets: []string{body}},
		{name: "docker login -pVALUE", input: "+ docker login -u ci -p" + v + " registry.example.invalid"},
		{name: "mysql -pVALUE", input: "+ mysql -uroot -h db -P 3306 -p" + v + " app"},
		{name: "curl -u user:password", input: "+ curl -fsS -u deploy:" + v + " https://registry.example.invalid/upload"},
		{name: "curl -u password starting with a hyphen", input: "+ curl -u 'deploy:-" + v + "' https://registry.example.invalid/upload"},
		{name: "openssl pass phrase starting with a hyphen", input: "+ openssl pkcs12 -in a.p12 -passin pass:-" + v},
		{name: "mysql attached password starting with a hyphen", input: "+ mysql -p-" + v + " app"},
		{name: "--password= value starting with a hyphen", input: "+ docker login --password=-" + v + " registry.example.invalid"},
		{name: "wget --http-password", input: "+ wget --http-user=ci --http-password=" + v + " https://registry.example.invalid/a"},
		{name: "single-quoted dollar password", input: "+ docker login --password '$" + v + "!' registry.example.invalid"},
		{name: "single-quoted dollar -p password", input: "+ docker login -p '$" + v + "' registry.example.invalid"},
		{name: "quoted password with spaces", input: "+ docker login --password ' violet meadow " + v + "' registry.example.invalid", secrets: []string{v, "violet meadow"}},
		{name: "double-quoted -p password with spaces", input: "+ sshpass -p \" violet " + v + "\" ssh host", secrets: []string{v}},
		{name: "vault --token", input: "+ vault login --token " + v},
		{name: "openssl pass phrase", input: "+ openssl pkcs12 -in a.p12 -passin pass:" + v},
		{name: "json authorization header", input: `request headers: {"Authorization":"Bearer ` + v + `"}`},
		{name: "python dict authorization header", input: `headers={'Authorization': 'Bearer ` + v + `'}`},
		{name: "yaml quoted authorization header", input: "headers:\n    Authorization: 'Bearer " + v + "'"},
		{name: "javascript authorization header", input: "fetch(url, { headers: { authorization: 'Bearer " + v + "' } })"},
		{name: "go header map", input: "request: map[Accept:[application/json] Authorization:[Bearer " + v + "]]"},
		{name: "json basic authorization header", input: `{"Authorization":"Basic ` + b + `"}`},
		{name: "authorization token scheme", input: "> Authorization: token " + v},
		{name: "authorization capitalized token scheme", input: "Authorization: Token " + v},
		{name: "authorization token auth-param", input: `> Authorization: Token token="` + v + `"`},
		{name: "authorization spaced auth-param", input: `> Authorization: Token token = "` + v + `"`},
		{name: "authorization unquoted spaced auth-param", input: `> Authorization: Custom key = ` + v},
		{name: "json-escaped authorization auth-param", input: `{"Authorization":"Token token=\"` + v + `\""}`},
		{name: "escaped serialized authorization header", input: `{"message":"{\"Authorization\":\"Basic ` + b + `\"}"}`, secrets: []string{b}},
		{name: "escaped serialized unknown scheme", input: `{"message":"{\"Authorization\":\"SSWS ` + v + `\"}"}`},
		{name: "authorization digest auth-params", input: `> Authorization: Digest username="ci", realm="registry", response="` + v + `"`},
		{name: "authorization unknown scheme", input: "> Authorization: SSWS " + v},
		{name: "authorization raw key", input: "> Authorization: " + v + " was rejected"},
		{name: "npmrc _auth", input: "//registry.example.invalid/:_auth=" + b, secrets: []string{b}},
		{name: "npmrc _auth letters only", input: "//registry.example.invalid/:_auth=dXNlcjpz" + "ZWNyZXQh", secrets: []string{"dXNlcjpz" + "ZWNyZXQh"}},
		{name: "docker config auth letters only", input: `{"auths":{"r":{"auth":"dXNlcjpz` + `ZWNyZXQh"}}}`, secrets: []string{"dXNlcjpz" + "ZWNyZXQh"}},
		{name: "docker config auth", input: `{"auths":{"registry.example.invalid":{"auth":"` + b + `"}}}`, secrets: []string{b}},
		{name: "kubeconfig client key data", input: "users:\n- name: ci\n  user:\n    client-key-data: " + body + "\n    client-certificate-data: " + b, secrets: []string{body, b}},
		{name: "azure storage connection string", input: "AZURE_STORAGE=DefaultEndpointsProtocol=https;AccountName=ci;AccountKey=" + body + "==;EndpointSuffix=core.windows.net", secrets: []string{body}},
		{name: "azure service bus connection string", input: "Endpoint=sb://ns.servicebus.windows.net/;SharedAccessKeyName=RootManageSharedAccessKey;SharedAccessKey=" + body + "=", secrets: []string{body}},
		{name: "add-mask trace", input: "+ echo '::add-mask::" + v + "'"},
		{name: "env auth assignment", input: "NEXUS_AUTH=" + b, secrets: []string{b}},
		{name: "quoted token in a diagnostic", input: `error: got token "` + v + `", want a valid session`},
		{name: "slack webhook", input: "POST https://hooks.slack.com/services/" + "T0" + "1ABCDEF/B0" + "1ABCDEF/" + body + " failed", secrets: []string{body}},
		{name: "quoted multiline password", input: "config:\n  password: \"violet meadow\n    copper kettle\n    crimson willow\n    amber forest\"\n  user: deploy-bot\nnext step",
			secrets: []string{"violet meadow", "copper kettle", "crimson willow", "amber forest"}, keep: []string{"  user: deploy-bot", "next step"}},
		{name: "single-quoted multiline password", input: "password: 'violet meadow\n  copper ''kettle''\n  amber forest'\nnext step",
			secrets: []string{"violet meadow", "kettle", "amber forest"}, keep: []string{"next step"}},
		{name: "diff replacing a quoted value's first line", input: "-password: \"old first part\n+password: \"new first part\n   violet meadow copper\n   amber forest kettle\"\nnext step",
			secrets: []string{"old first part", "new first part", "violet meadow copper", "amber forest kettle"}, keep: []string{"next step"}},
		{name: "diff replacing a quoted value's last line", input: " password: \"violet meadow\n-  copper kettle\"\n+  amber forest\"\nnext step",
			secrets: []string{"violet meadow", "copper kettle", "amber forest"}, keep: []string{"next step"}},
		{name: "diff replacing a quoted value's middle lines", input: " password: \"violet meadow\n-  copper kettle\n-  crimson willow\"\n+  amber forest\n+  golden harbor\"\n next: value",
			secrets: []string{"violet meadow", "copper kettle", "crimson willow", "amber forest", "golden harbor"}, keep: []string{" next: value"}},
		{name: "diff removing a sibling inside a block scalar", input: " password: |\n-  violet meadow\n-legacy: true\n+  copper kettle\n+  amber forest\n user: bot",
			secrets: []string{"violet meadow", "copper kettle", "amber forest"}, keep: []string{" user: bot"}},
		{name: "diff context key continuing a removed block", input: "-password: |\n+config:\n   client_secret: |\n\n+    copper kettle\n+    amber forest\n   user: bot\n-next: removed\n next: kept",
			secrets: []string{"copper kettle", "amber forest"}, keep: []string{" next: kept"}},
		{name: "diff added block scalar", input: "+ password: |\n+   violet meadow\n+   copper kettle\n+   amber forest\n  other: kept\nnext step",
			secrets: []string{"violet meadow", "copper kettle", "amber forest"}, keep: []string{"  other: kept", "next step"}},
		{name: "diff removed block scalar", input: "-password: >-\n-  violet meadow\n-  amber forest\n+password: ${{ secrets.DB }}\nnext step",
			secrets: []string{"violet meadow", "amber forest"}, keep: []string{"next step"}},
		{name: "diff context key with added body", input: " config:\n   client_secret: |\n+    violet meadow\n     amber forest\n   user: deploy-bot",
			secrets: []string{"violet meadow", "amber forest"}, keep: []string{"   user: deploy-bot"}},
		{name: "diff key continued on the next lines", input: "+ password:\n+   violet-meadow-copper-kettle\n+ user: deploy-bot",
			secrets: []string{"violet-meadow-copper-kettle"}, keep: []string{"+ user: deploy-bot"}},
	}, prefixCases)
	for i := range cases {
		if cases[i].secrets == nil {
			cases[i].secrets = []string{v}
		}
		// Blank lines keep the surrounding lines out of the credential's
		// continuation chain, which withholds a word directly adjacent to it.
		cases[i].input = "##[error]deploy failed\n\n" + cases[i].input + "\n\n--- FAIL: TestDeploy (0.01s)"
		cases[i].keep = append(cases[i].keep, "##[error]deploy failed", "--- FAIL: TestDeploy (0.01s)")
	}
	return cases
}

func TestRepositoryMonitorEvidenceTextWithholdsCICredentials(t *testing.T) {
	for _, tc := range repositoryMonitorEvidenceLeakCases() {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := repositoryMonitorEvidenceText(tc.input, 4096, 50)
			for _, secret := range tc.secrets {
				if strings.Contains(got, secret) {
					t.Fatalf("evidence text leaked %q: %q", secret, got)
				}
			}
			for _, keep := range tc.keep {
				if !strings.Contains(got, keep) {
					t.Fatalf("evidence text = %q, want %q kept", got, keep)
				}
			}
			if line, _ := repositoryMonitorEvidenceLine(tc.input, 4096); func() bool {
				for _, secret := range tc.secrets {
					if strings.Contains(line, secret) {
						return true
					}
				}
				return false
			}() {
				t.Fatalf("evidence line leaked a credential: %q", line)
			}
		})
	}
}

// TestRepositoryMonitorCIRepairPromptWithholdsCICredentials checks the
// assembled repair prompt of a monitor that opts in to CI free text, which
// namespace Task readers and the model see: the credential is in a check
// run's title, summary, and annotation.
func TestRepositoryMonitorCIRepairPromptWithholdsCICredentials(t *testing.T) {
	for _, tc := range repositoryMonitorEvidenceLeakCases() {
		t.Run(tc.name, func(t *testing.T) {
			run := repositoryMonitorEvidenceTestRun(701, "deploy", "completed", "failure", 1)
			output := run["output"].(map[string]any)
			output["title"], output["summary"] = tc.input, tc.input
			github := &repositoryMonitorRepairLabelsGitHub{
				runs: map[string][]map[string]any{"head51": {run}},
				annotations: map[int64][]map[string]any{701: {{
					"path": "deploy/config.yaml", "start_line": 1, "end_line": 1, "annotation_level": "failure", "title": tc.input, "message": tc.input,
				}}},
				statusObjects: map[string][]map[string]any{"head51": {{"id": 9101, "context": "deploy", "state": "failure", "description": tc.input}}},
			}
			reconciler, monitor := repositoryMonitorEvidenceTestReconciler(t, github, "ci-evidence-leaks")
			monitor.Spec.Repair.CIEvidence.Text = true
			prompt, err := reconciler.buildRepositoryMonitorRepairPrompt(context.Background(), monitor, repositoryMonitorCommandIntentFixCI, repositoryMonitorPullRequest{Number: 51, HeadSHA: "head51"}, &store.MonitorItem{})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(prompt, "TestDeploy") {
				t.Fatalf("prompt has no evidence text: %s", prompt)
			}
			for _, secret := range tc.secrets {
				if strings.Contains(prompt, secret) {
					t.Fatalf("repair prompt leaked %q:\n%s", secret, prompt)
				}
			}
		})
	}
}

// TestRepositoryMonitorEvidenceKeepsOrdinaryCIText guards the CI-evidence
// credential checks against withholding ordinary failure output.
func TestRepositoryMonitorEvidenceKeepsOrdinaryCIText(t *testing.T) {
	lines := []string{
		"##[error]Process completed with exit code 1.",
		"--- FAIL: TestLogin (0.02s)",
		"    login_test.go:41: expected status 200, got 401",
		"+ docker login -u ci --password-stdin registry.example.invalid",
		"+ docker login -u ci -p *** registry.example.invalid",
		"+ docker login -u ci --password $REGISTRY_PASSWORD registry.example.invalid",
		"+ curl -fsS -u \"$USER:$PASS\" https://registry.example.invalid/upload",
		"Run docker login --password \"${{ secrets.REGISTRY_PASSWORD }}\" registry.example.invalid",
		"+ sshpass -p \"$SSH_PASSWORD\" ssh host",
		"+ mkdir -p /tmp/build",
		"+ go test -p 4 ./...",
		"+ docker run -u 1000:1000 --rm image:tag",
		"+ docker buildx build --secret id=npmrc,src=.npmrc .",
		`syntax error: unexpected token "identifier"`,
		"error: Authorization: header is required",
		"Authorization: Bearer ***",
		"oauth: enabled",
		"basic_auth: true",
		"auth: required",
	}
	input := strings.Join(lines, "\n")
	got, _ := repositoryMonitorEvidenceText(input, 4096, 50)
	for _, line := range lines {
		if !strings.Contains(got, line) {
			t.Fatalf("evidence text = %q, want %q kept", got, line)
		}
	}
}

func TestRepositoryMonitorEvidenceURLDropsWebhookPaths(t *testing.T) {
	body := repositoryMonitorEvidenceLeakBody
	for _, raw := range []string{
		"https://hooks.slack.com/services/" + "T0" + "1ABCDEF/B0" + "1ABCDEF/" + body,
		"https://discord.com/api/webhooks/1234567890/" + body,
	} {
		if got := repositoryMonitorEvidenceURL(raw); got != "" {
			t.Fatalf("evidence URL = %q, want a webhook URL dropped", got)
		}
	}
	if got := repositoryMonitorEvidenceURL("https://ci.example/job/12/console"); got != "https://ci.example/job/12/console" {
		t.Fatalf("evidence URL = %q, want a plain link kept", got)
	}
}

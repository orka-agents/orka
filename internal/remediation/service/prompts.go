package service

import (
	"encoding/json"
	"strings"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/intake"
	"github.com/orka-agents/orka/internal/remediation/source"
)

const (
	SelectionMarker         = "ORKA_REMEDIATION_SELECT_V1"
	ChecksMarker            = "ORKA_REMEDIATION_CHECKS_V1"
	ReproductionMarker      = "ORKA_REMEDIATION_REPRODUCE_V1"
	GenerationMarker        = "ORKA_REMEDIATION_GENERATE_V1"
	VerificationMarker      = "ORKA_REMEDIATION_VERIFY_V1"
	observationInstructions = "An observedStdout contains only frozen HTTP status/body or application API/TCP observation bytes whose SHA-256, byte length, " +
		"and exit code exactly match a completed runner observation. It is reconstructed from that exact match, " +
		"not a predicted response. Missing observedStdout is unavailable; never substitute an expectation or a conclusion label. "
	applicationNormalReviewInstructions = "For non-replica application API normal controls, normalValue is the declared public " +
		"healthy value, not observed status. Inspect the chosen apiBaseline and any matching actor initialAPI receipts. " +
		"The final exact pointer/UID must match, while both resourceVersion and reduced-status digest change from the " +
		"authoritative Create/latest matching actor response. A later GET, an absent target, a resourceVersion-only change, " +
		"or a vacuous/default value cannot establish normal reconciliation. Explicit false/zero is legitimate only as a " +
		"genuinely non-initial healthy result. These rules do not replace replica descendant or TCP-positive controls. "
)

func prompt(marker, instructions string, data any) string {
	encoded, _ := json.Marshal(data)
	return marker + "\n" + instructions + "\nUntrusted input JSON:\n" + string(encoded)
}

type promptInventoryEntry struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

func selectionPrompt(report intake.Report, target source.Target, inventory []source.Entry) string {
	omitTests := len(inventory) > 64
	regular := make([]promptInventoryEntry, 0, len(inventory))
	for _, entry := range inventory {
		if entry.Mode == "100644" || entry.Mode == "100755" {
			if omitTests && (strings.HasPrefix(entry.Path, "tests/") || strings.HasSuffix(entry.Path, "_test.go")) {
				continue
			}
			regular = append(regular, promptInventoryEntry{Path: entry.Path, Size: entry.Size})
		}
	}
	return prompt(SelectionMarker,
		"Select 1-32 regular text files (at most 64 KiB total size across selected files) needed to understand the reported behavior and ordinary behavior. "+
			"Return exactly {\"files\":[\"relative/path\"]}. Select only inventory paths; no directories, tools, or commands. "+
			"The source identity is already frozen and cannot be changed.",
		struct {
			Report    intake.Report          `json:"report"`
			Target    source.Target          `json:"target"`
			Inventory []promptInventoryEntry `json:"inventory"`
		}{report, target, regular})
}

func checksPrompt(report intake.Report, packet source.Packet) string {
	return prompt(ChecksMarker,
		"Propose independently observable checks for the report and normal behavior on this exact source. "+
			"Return {\"scope\":[\"...\"],\"gaps\":[],\"requirements\":[{\"kind\":\"process\",\"name\":\"python3\",\"description\":\"...\"}],"+
			"\"files\":[{\"path\":\"server.py\",\"content\":\"...\",\"executable\":true}],"+
			"\"checks\":[{\"id\":\"...\",\"kind\":\"reproduction or normal\",\"command\":[], "+
			"\"http\":{\"version\":1,\"serverCommand\":[\"/checks/server.py\"],\"path\":\"/...\"},"+
			"\"healthy\":{\"exitCode\":0,\"stdout\":\"{\\\"status\\\":200,\\\"body\\\":\\\"...\\\"}\"},"+
			"\"failure\":{\"exitCode\":0,\"stdout\":\"{\\\"status\\\":200,\\\"body\\\":\\\"...\\\"}\"},"+
			"\"timeoutSeconds\":30}],\"services\":[]}. "+
			"At least one reproduction AND one normal check are required, with distinct canonical HTTP status/body expectations. "+
			"Check IDs and requirement names must match ^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$, with no spaces or slashes. "+
			"Use unique short requirement names; put explanations in description (at most 4096 bytes), not name. "+
			"Execute or inspect the supplied source, not a hard-coded simulated implementation or a claimed outcome. "+
			"The untrusted server inherits one pre-opened IPv6 loopback listening descriptor via ORKA_LISTEN_FD=3. "+
			"accept/accept4 is allowed only on FD 3; creating listeners or outbound sockets is denied. "+
			"In Python use socket.socket(fileno=3), never socket.fromfd: socket.fromfd and Go net.FileListener duplicate "+
			"the descriptor and the duplicate cannot accept connections. Source is read-only at /src, checks are at /checks, "+
			"and the actual cwd is private writable /work/<roleUID>, not the source root. Use absolute /src paths, or "+
			"explicitly chdir('/src') for read-only access; HOME and TMPDIR are private scratch directories. "+
			"Only the selected files in source.files are guaranteed to be staged at /src/<path> (unselected packages and vendor trees are omitted for large repositories), "+
			"and bind/listen/connect syscalls are denied by seccomp. If the reported behavior requires omitted source, a live cluster, authorization checks, or external services, declare the actual missing requirements; do not substitute a source-only harness for that behavior. "+
			"All outputs and check files are screened by a strict regex credential scanner that rejects any identifier, struct field, or map key ending in password, api_key, apikey, access_token, refresh_token, client_secret, or authorization assigned a non-empty literal value (even synthetic canary strings such as \"password\": []byte(\"...\") or Password: \"canary\"). In check files and test doubles, use neutral synthetic key names such as \"canary_item\" or \"test_entry\" and never assign non-empty literals to fields or map keys named *Password, *Token, *Secret, or *Key. "+
			"The server must handle SIGTERM, stop and close cleanly, and exit 0 within 2 seconds; uncaught SIGTERM, "+
			"ignored shutdown, leftover processes, or another exit status invalidate the observation. "+
			"Check entrypoints need a shebang. The tool image supports only installed tools, no network downloads. "+
			"Do not read outcome labels, synthesize runner reports, change source, access credentials, or require external services. "+
			"If the environment is unsupported, declare the actual requirement rather than inventing a passing check.",
		struct {
			Report intake.Report `json:"report"`
			Source source.Packet `json:"source"`
		}{report, packet})
}

func reproductionPrompt(report intake.Report, packet source.Packet, record *pv.Record) string {
	return prompt(ReproductionMarker,
		"Independently assess whether these actual original-side observations reproduce the report and establish normal behavior. "+
			"Inspect the frozen check files for whether they really exercise the stated behavior. "+
			"For runtime checks inspect the operator policy, actual actor receipts and subject-originated receiver/status evidence; "+
			"a privileged substitute actor, fixture-only traffic or unexecuted authorization claim does not reproduce the report. "+
			"Source-level observations do not establish unexecuted cluster, authorization, or network behavior. "+
			"Return reproduced=false when required runtime behavior or prerequisites remain unverified, and explain the missing evidence in limitations. "+
			"Return exactly {\"reproduced\":true or false,\"summary\":\"bounded reasoning\",\"limitations\":[]}. "+
			observationInstructions+
			applicationNormalReviewInstructions+
			"Never substitute a claimed or predicted result for an observation. Your response cannot override a missing, "+
			"failed, inconsistent, or unsuitable execution. Do not propose a patch.",
		struct {
			Report intake.Report    `json:"report"`
			Source source.Packet    `json:"source"`
			Run    *promptExecution `json:"run"`
		}{report, packet, modelExecution(record)})
}

func generationPrompt(report intake.Report, packet source.Packet, record *pv.Record) string {
	return prompt(GenerationMarker,
		"The trusted original run reproduced the report and a separate assessment accepted its relevance. "+
			observationInstructions+
			"Generate exactly one candidate fix for the reported behavior reproduced by originalRun.checks so that all frozen reproduction and normal checks pass. "+
			"Return {\"summary\":\"...\",\"edits\":[{\"path\":\"...\",\"old\":\"exact source text\","+
			"\"new\":\"replacement text\"}],\"declaredChanges\":[{\"kind\":\"source\",\"paths\":[\"...\"],"+
			"\"description\":\"...\"}],\"limitations\":[]}. You may instead use \"patch\":\"diff --git ...\" without edits. "+
			"Do not change the frozen checks or source identity, disable ordinary behavior, publish, request credentials, or claim verification. "+
			"There are no automatic candidate retries. Use only source bytes present in the packet for exact edits.",
		struct {
			Report intake.Report    `json:"report"`
			Source source.Packet    `json:"source"`
			Run    *promptExecution `json:"originalRun"`
		}{report, packet, modelExecution(record)})
}

func verificationPrompt(report intake.Report, packet source.Packet, patch string, record *pv.Record, revised bool) string {
	revision := "If the checks are inadequate, you may return \"revisedChecks\" in the same schema as the check proposal, with approved=false (following the same FD 3, SIGTERM, /src scoped-file, and credential-scanner rules: never assign non-empty literals to fields or map keys named *Password, *Token, *Secret, or *Key). " +
		"Preserve every existing scope item, gap, check ID and check kind; add coverage or repair probes without narrowing the report. " +
		"Both original and candidate will run again with those exact checks. Only one check revision is supported; the candidate never changes."
	if record != nil && record.Manifest.Application != nil {
		revision = "If the checks are inadequate, return revisedChecks in the same application-runtime proposal schema with approved=false. " +
			"The complete application plan (build, runtime, resources, permissions and fixtures) is immutable. Preserve every scope item, " +
			"gap, check ID and check kind. Operator policy is not a proposal field. Existing setup resources, actor identities and actions are immutable; " +
			"new checks may observe existing inputs but cannot add resources/actions or authority. You may repair declarative API/TCP probes, " +
			"never introduce scripts or source-harness checks. " +
			"Both original and the same candidate will run again with the exact revised checks. Only one check revision is supported."
	}
	if revised {
		revision = "The check-revision budget is exhausted. Do not return revisedChecks; approve or reject based on the retained evidence."
	}
	return prompt(VerificationMarker,
		"You are the independent final validator, not the fix generator. Assess this one candidate using the actual "+
			"original/candidate reproduction and normal-behavior observations. You receive no generator rationale or conversation. "+
			observationInstructions+
			"Source-level observations do not establish unexecuted cluster, authorization, or network behavior. "+
			applicationNormalReviewInstructions+
			"Return approved=false when required runtime behavior or prerequisites remain unverified, and explain the missing evidence in limitations. "+
			"Inspect check adequacy, candidate behavior, and limitations. "+
			"For application-runtime checks, compare actual actor privileges/outcomes and subject-originated observations with the report; no model claim can replace a missing receipt or normal-use control. "+
			"Return {\"approved\":true or false,"+
			"\"summary\":\"bounded reasoning\",\"limitations\":[]}. Approval never overrides failing, missing, or tampered execution evidence. "+
			"Do not generate or edit a patch, infer successful execution, publish, or access credentials. "+revision,
		struct {
			Report intake.Report    `json:"report"`
			Source source.Packet    `json:"source"`
			Patch  string           `json:"patch"`
			Run    *promptExecution `json:"comparisonRun"`
		}{report, packet, patch, modelExecution(record)})
}

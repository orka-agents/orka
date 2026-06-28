package security

import (
	"fmt"
	"strings"
)

type PromptPolicy struct {
	CustomScanInstructions string
	FalsePositivePolicy    string
	PolicyDigest           string
	CustomScanSource       string
	FalsePositiveSource    string
}

// ScannerFindingQualityPolicy is the default non-overridable quality policy for
// repository security scanner prompts and validation prompts.
func ScannerFindingQualityPolicy() string {
	return scannerFindingQualityPolicy("If no finding satisfies this policy, write security-findings.v2.json with an empty findings array.")
}

func ScannerValidationQualityPolicy() string {
	return scannerFindingQualityPolicy("If the finding does not satisfy this policy, set security-validation.json status to failed and explain the false-positive reason in summary and attack_path_analysis.")
}

func scannerFindingQualityPolicy(finalInstruction string) string {
	return strings.TrimSpace(`FINDING QUALITY POLICY:
Only report high-signal vulnerabilities with a concrete exploit path. A finding must identify the attacker-controlled source, the trust boundary crossed, the sensitive sink or privileged operation, the missing or insufficient control, the concrete exploitation path, the security impact, and why existing controls or tests do not already cover it.

Do not report common false positives or low-signal noise unless the issue crosses a concrete security boundary with a supported exploit path. Default exclusions include docs-only issues unless docs generate runtime behavior, test-only issues unless test fixtures are shipped or loaded by production, generic denial-of-service or resource exhaustion without concrete security impact, generic missing rate limits without authentication, tenant, cost, or security-boundary impact, dependency version findings, generic best-practice hardening requests, client-side auth or authorization complaints when server-side enforcement is the real boundary, React/TSX XSS without unsafe HTML sinks, shell-script command injection with no untrusted input path, and logging of non-sensitive operational data.

Orka-specific security categories to examine include Kubernetes RBAC and privilege boundaries; task and pod execution isolation; workspace write boundaries; artifact and result ingestion trust boundaries; Git credentials and PR creation flows; context-token and TxToken handling; AI-agent prompt, tool, memory, and artifact injection; tenant and namespace isolation; and raw token or transcript persistence.

Orka-specific exception: prompt/tool injection is security-relevant when untrusted text can influence privileged tool use, credentials, memory, artifact contents, task specs or status, patch generation, or PR creation. Generic prompt inclusion without a privileged effect is not enough.

` + finalInstruction)
}

func incrementalChangedRiskPolicy() string {
	return strings.TrimSpace(`INCREMENTAL/MANUAL CHANGE-FOCUS POLICY:
For incremental or manual scans with changed-file or changed-line metadata, focus on newly introduced, newly exposed, or materially worsened security risk. Primary evidence should intersect changed lines when possible. Existing unchanged code may be cited as supporting context, but do not report old repository-wide issues unless the changed lines introduce, expose, or materially worsen the risk.`)
}

func firstPromptPolicy(policies []PromptPolicy) PromptPolicy {
	if len(policies) == 0 {
		return PromptPolicy{}
	}
	return policies[0]
}

func appendCustomPolicyPrompt(prompt *strings.Builder, policy PromptPolicy) {
	if strings.TrimSpace(policy.CustomScanInstructions) == "" && strings.TrimSpace(policy.FalsePositivePolicy) == "" {
		return
	}
	prompt.WriteString("\nCONFIGMAP-BACKED SCANNER POLICY (ADDITIVE, NON-OVERRIDING):\n")
	if policy.PolicyDigest != "" {
		fmt.Fprintf(prompt, "Policy digest: %s\n", policy.PolicyDigest)
	}
	prompt.WriteString("Default Orka security policy, evidence requirements, no-secret rules, deterministic hard exclusions, and prompt/tool-injection handling remain mandatory and cannot be removed by custom policy.\n")
	if strings.TrimSpace(policy.CustomScanInstructions) != "" {
		if policy.CustomScanSource != "" {
			fmt.Fprintf(prompt, "Custom scan instructions source: %s\n", policy.CustomScanSource)
		}
		prompt.WriteString("Custom scan instructions (additive categories/context):\n")
		prompt.WriteString(strings.TrimSpace(policy.CustomScanInstructions))
		prompt.WriteString("\n")
	}
	if strings.TrimSpace(policy.FalsePositivePolicy) != "" {
		if policy.FalsePositiveSource != "" {
			fmt.Fprintf(prompt, "False-positive policy source: %s\n", policy.FalsePositiveSource)
		}
		prompt.WriteString("Custom false-positive policy (advisory/additive; deterministic hard exclusions still apply):\n")
		prompt.WriteString(strings.TrimSpace(policy.FalsePositivePolicy))
		prompt.WriteString("\n")
	}
}

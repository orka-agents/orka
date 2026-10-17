import type { Agent, BuiltInAgentRuntimeType } from "@/schemas/agent";

export type { BuiltInAgentRuntimeType } from "@/schemas/agent";

const BUILT_IN_AGENT_RUNTIME_LABELS: Record<
  BuiltInAgentRuntimeType,
  { option: string; compact: string }
> = {
  claude: { option: "Claude ACP", compact: "Claude ACP" },
  codex: { option: "OpenAI Codex ACP", compact: "Codex ACP" },
  copilot: { option: "GitHub Copilot ACP", compact: "Copilot ACP" },
  opencode: { option: "OpenCode ACP", compact: "OpenCode ACP" },
};

export const OPENCODE_REVIEWED_RUNTIME_DEFAULTS = {
  defaultAllowedTools: ["Read", "Write", "Edit", "Bash", "Glob", "Grep"],
  defaultAllowBash: true,
} as const;

export function builtInAgentRuntimeLabel(type: BuiltInAgentRuntimeType) {
  return BUILT_IN_AGENT_RUNTIME_LABELS[type].compact;
}

// The provider an Agent resolves through: its Provider resource reference, or
// the legacy inline model.provider when no reference is set.
export function agentProviderName(agent: Agent) {
  return agent.spec.providerRef?.name || agent.spec.model?.provider || undefined;
}

// Readiness comes from the controller's Ready condition; an Agent that has not
// been reconciled yet has none and reports no readiness either way.
export function agentReadiness(agent: Agent) {
  const condition = agent.status?.conditions?.find((c) => c.type === "Ready");
  if (!condition) return undefined;
  return { ready: condition.status === "True", message: condition.message };
}

export function isOpenCodeModelID(value: string) {
  const normalized = value.trim();
  const slash = normalized.indexOf("/");
  return (
    slash > 0 &&
    slash < normalized.length - 1 &&
    !normalized.includes("{") &&
    !normalized.includes("}")
  );
}

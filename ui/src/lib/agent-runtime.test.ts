import { describe, expect, it } from "vitest";
import {
  OPENCODE_REVIEWED_RUNTIME_DEFAULTS,
  agentProviderName,
  agentReadiness,
  builtInAgentRuntimeLabel,
  isOpenCodeModelID,
} from "./agent-runtime";

describe("built-in agent runtime display", () => {
  it("uses consistently capitalized compact labels", () => {
    expect(builtInAgentRuntimeLabel("claude")).toBe("Claude ACP");
    expect(builtInAgentRuntimeLabel("codex")).toBe("Codex ACP");
    expect(builtInAgentRuntimeLabel("copilot")).toBe("Copilot ACP");
    expect(builtInAgentRuntimeLabel("opencode")).toBe("OpenCode ACP");
  });

  it("names the provider from providerRef before the legacy model.provider", () => {
    expect(agentProviderName({ metadata: { name: "a" }, spec: { providerRef: { name: "prod" }, model: { provider: "openai" } } })).toBe("prod");
    expect(agentProviderName({ metadata: { name: "a" }, spec: { model: { provider: "openai" } } })).toBe("openai");
    expect(agentProviderName({ metadata: { name: "a" }, spec: {} })).toBeUndefined();
  });

  it("derives readiness from the Ready condition only", () => {
    const base = { metadata: { name: "a" }, spec: {} };
    expect(agentReadiness(base)).toBeUndefined();
    expect(agentReadiness({ ...base, status: { activeTasks: 0, conditions: [{ type: "Ready", status: "True" }] } })).toEqual({ ready: true, message: undefined });
    expect(
      agentReadiness({ ...base, status: { activeTasks: 0, conditions: [{ type: "Ready", status: "False", message: 'referenced provider "x" not found' }] } }),
    ).toEqual({ ready: false, message: 'referenced provider "x" not found' });
  });

  it("defines the reviewed OpenCode native-tool policy", () => {
    expect(OPENCODE_REVIEWED_RUNTIME_DEFAULTS).toEqual({
      defaultAllowedTools: ["Read", "Write", "Edit", "Bash", "Glob", "Grep"],
      defaultAllowBash: true,
    });
  });

  it("validates literal OpenCode provider/model IDs", () => {
    expect(isOpenCodeModelID("openai/gpt-5.4")).toBe(true);
    expect(isOpenCodeModelID("gpt-5.4")).toBe(false);
    expect(isOpenCodeModelID("{env:TOKEN}/gpt")).toBe(false);
    expect(isOpenCodeModelID("openai/")).toBe(false);
  });
});

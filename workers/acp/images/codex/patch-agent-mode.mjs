import {readFileSync, writeFileSync} from "node:fs";

const path = process.argv[2];
if (!path) throw new Error("AgentMode.ts path is required");
let source = readFileSync(path, "utf8");

const modeAnchor = `    static readonly AgentFullAccess = new AgentMode(
        "agent-full-access",
        "Agent (full access)",
        "Codex can edit files outside this workspace and run commands with network access. Exercise caution when using.",
        "never",
        {"type": "dangerFullAccess"},
        "danger-full-access"
    );
`;
const externalMode = `${modeAnchor}    static readonly OrkaExternal = new AgentMode(
        "orka-external",
        "Orka external sandbox",
        "Execution is confined by the Orka RuntimeSession boundary.",
        "on-request",
        {"type": "externalSandbox", networkAccess: "restricted"},
        "read-only"
    );
`;
if (!source.includes(modeAnchor)) throw new Error("AgentFullAccess anchor not found");
source = source.replace(modeAnchor, externalMode);

const allAnchor = `        return [AgentMode.ReadOnly, AgentMode.Agent, AgentMode.AgentFullAccess];`;
const allReplacement = `        return [AgentMode.ReadOnly, AgentMode.Agent, AgentMode.AgentFullAccess, AgentMode.OrkaExternal];`;
if (!source.includes(allAnchor)) throw new Error("AgentMode.all anchor not found");
source = source.replace(allAnchor, allReplacement);

writeFileSync(path, source);

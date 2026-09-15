import {readFileSync, writeFileSync} from "node:fs";

const path = process.argv[2];
const clientPath = process.argv[3];
if (!path || !clientPath) throw new Error("AgentMode.ts and CodexAcpClient.ts paths are required");
let source = readFileSync(path, "utf8");
let clientSource = readFileSync(clientPath, "utf8");

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

// Explicit tool policies accept only the controller's session configuration.
// Codex skips untrusted project config layers while retaining the separately
// selected Orka external sandbox. Omitted policies keep upstream trust behavior.
const trustAnchor = `            projects: Object.fromEntries(sessionRoots.map(root => [root, {
                trust_level: "trusted",
            }])),`;
const trustReplacement = `            projects: Object.fromEntries(sessionRoots.map(root => [root, {
                trust_level: process.env["ORKA_CODEX_DISABLE_PROJECT_CONFIG"] === "1" ? "untrusted" : "trusted",
            }])),`;
if (!clientSource.includes(trustAnchor)) throw new Error("CodexAcpClient project trust anchor not found");
clientSource = clientSource.replace(trustAnchor, trustReplacement);

writeFileSync(path, source);
writeFileSync(clientPath, clientSource);

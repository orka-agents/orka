import {readFileSync, writeFileSync} from "node:fs";

const path = process.argv[2];
if (!path) throw new Error("AgentMode.ts path is required");
let source = readFileSync(path, "utf8");

const modeAnchor = `    static readonly AgentFullAccess = new AgentMode(
        "agent-full-access",
        "Full access",
        "Unrestricted access to the internet and any file on your computer",
        "full_access",
        "never",
        "user",
        {"type": "dangerFullAccess"},
        "danger-full-access",
    );
`;
const externalMode = `${modeAnchor}    static readonly OrkaExternal = new AgentMode(
        "orka-external",
        "Orka external sandbox",
        "Execution is confined by the Orka RuntimeSession boundary.",
        "standard",
        "on-request",
        "user",
        {"type": "externalSandbox", networkAccess: "restricted"},
        "read-only",
    );
`;
if (!source.includes(modeAnchor)) throw new Error("AgentFullAccess anchor not found");
source = source.replace(modeAnchor, externalMode);

const allAnchor = `        return [AgentMode.ReadOnly, AgentMode.WorkspaceWrite, AgentMode.Agent, AgentMode.AgentFullAccess];`;
const allReplacement = `        return [AgentMode.ReadOnly, AgentMode.WorkspaceWrite, AgentMode.Agent, AgentMode.AgentFullAccess, AgentMode.OrkaExternal];`;
if (!source.includes(allAnchor)) throw new Error("AgentMode.all anchor not found");
source = source.replace(allAnchor, allReplacement);

writeFileSync(path, source);

// The adapter marks every session root as a trusted Codex project, which makes
// Codex load the cloned repository's .codex/config.toml. Its mcp_servers and
// shell_environment_policy entries then run repository commands outside the
// Orka tool policy. Grant no trust instead, so Codex treats the root like any
// unknown directory: project config stays disabled while AGENTS.md and the
// default approval behavior remain. An explicit "untrusted" level would also
// drop AGENTS.md. The supervisor rewrites CODEX_HOME/config.toml for every
// session, so no trust entry can carry over from an earlier session.
const clientPath = process.argv[3];
if (!clientPath) throw new Error("CodexAcpClient.ts path is required");
let client = readFileSync(clientPath, "utf8");
const trustAnchor = `            projects: Object.fromEntries(sessionRoots.map(root => [root, {
                trust_level: "trusted",
            }])),
`;
const trustReplacement = `            projects: {},
`;
if (client.split(trustAnchor).length !== 2) throw new Error("session root trust anchor not found exactly once");
client = client.replace(trustAnchor, trustReplacement);
writeFileSync(clientPath, client);

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

// The mode is controlled by the supervisor, not by workspace configuration.
// Pin it before app-server initialization as well as in the per-thread config.
const {dirname, join} = await import("node:path");
const connectionPath = join(dirname(path), "CodexJsonRpcConnection.ts");
let connection = readFileSync(connectionPath, "utf8");
const envAnchor = `    const spawnEnv = env ?? process.env;`;
const strictArgs = `${envAnchor}
    const codexArgs = ['app-server'];
    if (spawnEnv["ORKA_CODEX_HYPERLIGHT_ONLY"] === "1") {
        codexArgs.push('-c', 'notify=[]');
        for (const feature of ["shell_tool", "hooks", "plugins", "multi_agent", "multi_agent_v2", "code_mode", "code_mode_host"]) {
            codexArgs.push('-c', \`features.\${feature}=false\`);
        }
        codexArgs.push('-c', \`projects={\${JSON.stringify(process.cwd())}={trust_level="untrusted"}}\`);
    }`;
if (!connection.includes(envAnchor)) throw new Error("Codex spawn environment anchor not found");
connection = connection.replace(envAnchor, strictArgs);
const nativeSpawn = `spawn(codexPath, ['app-server'], { env: spawnEnv })`;
const bundledSpawn = `spawn(process.execPath, [bundledCodexPath, 'app-server'], {env: spawnEnv})`;
if (!connection.includes(nativeSpawn) || !connection.includes(bundledSpawn)) throw new Error("Codex spawn anchors not found");
connection = connection.replace(nativeSpawn, `spawn(codexPath, codexArgs, { env: spawnEnv })`);
connection = connection.replace(bundledSpawn, `spawn(process.execPath, [bundledCodexPath, ...codexArgs], {env: spawnEnv})`);
writeFileSync(connectionPath, connection);

const clientPath = join(dirname(path), "CodexAcpClient.ts");
let client = readFileSync(clientPath, "utf8");
const trustAnchor = `                trust_level: "trusted",`;
if (!client.includes(trustAnchor)) throw new Error("Codex project trust anchor not found");
client = client.replace(trustAnchor, `                trust_level: process.env["ORKA_CODEX_HYPERLIGHT_ONLY"] === "1" ? "untrusted" : "trusted",`);
const noServersAnchor = `        if (mcpServers.length === 0) {
            return configWithWorkspaceRoots;
        }`;
if (!client.includes(noServersAnchor)) throw new Error("Codex MCP empty-list anchor not found");
client = client.replace(noServersAnchor, `        const strictMcp = process.env["ORKA_CODEX_HYPERLIGHT_ONLY"] === "1";
        if (mcpServers.length === 0 && !strictMcp) {
            return configWithWorkspaceRoots;
        }`);
const requestedServersAnchor = `        let serversToConfigure = requestedServers;`;
if (!client.includes(requestedServersAnchor)) throw new Error("Codex MCP configuration anchor not found");
client = client.replace(requestedServersAnchor, `        if (strictMcp) {
            // Config reads do not start servers. Disable every effective server
            // that was not supplied by the frozen Orka session configuration.
            const current = await this.codexClient.configRead({ includeLayers: false, cwd: projectPath });
            const existing = current?.config?.["mcp_servers"];
            const names = isJsonObject(existing) ? Object.keys(existing) : [];
            if (requestedServers.some(server => names.includes(server.name))) {
                throw RequestError.invalidRequest("Unmanaged MCP configuration conflicts with an Orka server");
            }
            return {
                ...configWithWorkspaceRoots,
                mcp_servers: {
                    ...Object.fromEntries(names.map(name => [name, { enabled: false }])),
                    ...Object.fromEntries(requestedServers.map(mcp => [mcp.name, this.createMcpSeverConfig(mcp.server)])),
                },
            };
        }
        let serversToConfigure = requestedServers;`);
// Untrusted projects deliberately suppress Codex's automatic AGENTS loading.
// Snapshot the current workspace's declarative instructions into its private
// CODEX_HOME so command-bearing project configuration can stay disabled.
const rootsAnchor = `        const sessionRoots = [projectPath, ...additionalDirectories];`;
if (!client.includes(rootsAnchor)) throw new Error("Codex instruction snapshot anchor not found");
client = client.replace(rootsAnchor, `${rootsAnchor}
        if (process.env["ORKA_CODEX_HYPERLIGHT_ONLY"] === "1") {
            const files = await import("node:fs/promises");
            const { constants } = await import("node:fs");
            const home = process.env["CODEX_HOME"];
            if (!home) throw RequestError.invalidRequest("Strict Codex requires a private CODEX_HOME");
            let text: string | undefined;
            for (const name of ["AGENTS.override.md", "AGENTS.md"]) {
                let file;
                try {
                    file = await files.open(path.join(projectPath, name), constants.O_RDONLY | constants.O_NOFOLLOW);
                    if (!(await file.stat()).isFile()) continue;
                    const buffer = Buffer.alloc(32768);
                    const { bytesRead } = await file.read(buffer, 0, buffer.length, 0);
                    text = buffer.subarray(0, bytesRead).toString("utf8");
                    if (text.trim()) break;
                } catch (error) {
                    if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
                } finally {
                    await file?.close();
                }
            }
            const target = path.join(home, "AGENTS.override.md");
            if (text?.trim()) {
                const output = await files.open(target, constants.O_WRONLY | constants.O_CREAT | constants.O_TRUNC | constants.O_NOFOLLOW, 0o600);
                try { await output.writeFile(text); } finally { await output.close(); }
            } else {
                await files.rm(target, { force: true });
            }
        }`);
// The adapter gives an HTTP MCP server only its URL and headers. For sessions
// that can call sandbox_exec, the supervisor sets how long Codex waits for
// one tool call.
const httpServerAnchor = `                        "http_headers": Object.fromEntries(mcpServer.headers.map(h => [h.name, h.value])),
                    }`;
if (!client.includes(httpServerAnchor)) throw new Error("Codex HTTP MCP server anchor not found");
client = client.replace(httpServerAnchor, `                        "http_headers": Object.fromEntries(mcpServer.headers.map(h => [h.name, h.value])),
                        ...(process.env["ORKA_CODEX_MCP_TOOL_TIMEOUT_SEC"]
                            ? { "tool_timeout_sec": Number(process.env["ORKA_CODEX_MCP_TOOL_TIMEOUT_SEC"]) }
                            : {}),
                    }`);
writeFileSync(clientPath, client);

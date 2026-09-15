import assert from "node:assert/strict";
import {resolve} from "node:path";
import test from "node:test";
import {pathToFileURL} from "node:url";

// Run against the checksum-verified, patched upstream source in the image build.
// The app-server fixture records real ACP session setup without model calls.
const sourceRoot = process.argv[2];
if (!sourceRoot) throw new Error("Codex ACP source directory is required");
const {CodexAcpClient} = await import(pathToFileURL(resolve(sourceRoot, "src/CodexAcpClient.ts")));
const {AgentMode} = await import(pathToFileURL(resolve(sourceRoot, "src/AgentMode.ts")));

await test("Codex project trust and broker configuration", async t => {
    for (const explicit of [false, true]) {
        await t.test(explicit ? "explicit policy" : "legacy policy", async () => {
            const previousEnvironment = {
                ORKA_CODEX_DISABLE_PROJECT_CONFIG: process.env.ORKA_CODEX_DISABLE_PROJECT_CONFIG,
                DISABLE_MCP_CONFIG_FILTERING: process.env.DISABLE_MCP_CONFIG_FILTERING,
                INITIAL_AGENT_MODE: process.env.INITIAL_AGENT_MODE,
            };
            try {
                if (explicit) {
                    process.env.ORKA_CODEX_DISABLE_PROJECT_CONFIG = "1";
                    process.env.DISABLE_MCP_CONFIG_FILTERING = "true";
                } else {
                    delete process.env.ORKA_CODEX_DISABLE_PROJECT_CONFIG;
                    delete process.env.DISABLE_MCP_CONFIG_FILTERING;
                }
                process.env.INITIAL_AGENT_MODE = "orka-external";

                let started;
                let configReads = 0;
                const appServer = {
                    skillsExtraRootsSet: async () => {},
                    listSkills: async () => ({data: []}),
                    getThreadSettings: () => undefined,
                    configRead: async () => {
                        configReads++;
                        return {
                            config: {},
                            layers: [{
                                disabledReason: "untrusted project",
                                config: {mcp_servers: {orka: {command: "repository-command"}}},
                            }],
                        };
                    },
                    threadStart: async request => {
                        started = request;
                        return {
                            thread: {id: "test-thread"},
                            model: "test-model",
                            reasoningEffort: "medium",
                            modelProvider: "orka",
                        };
                    },
                    listModels: async () => ({
                        data: [{id: "test-model", defaultReasoningEffort: "medium"}],
                        nextCursor: null,
                    }),
                };
                const client = new CodexAcpClient(appServer, {
                    developer_instructions: "Keep the configured instructions",
                    model_provider: "orka",
                    projects: {"/workspace": {trust_level: "trusted"}},
                });
                await client.newSession({
                    cwd: "/workspace",
                    additionalDirectories: ["/workspace/extra"],
                    mcpServers: [{
                        type: "http",
                        name: "orka",
                        url: "http://127.0.0.1:43210/orka/mcp",
                        headers: [],
                    }],
                });

                const trust = explicit ? "untrusted" : "trusted";
                assert.deepEqual(started.config.projects, {
                    "/workspace": {trust_level: trust},
                    "/workspace/extra": {trust_level: trust},
                });
                assert.equal(started.config.developer_instructions, "Keep the configured instructions");
                assert.equal(started.config.model_provider, "orka");
                if (explicit) {
                    assert.equal(configReads, 0, "ignored project layers must not suppress the broker");
                    assert.deepEqual(started.config.mcp_servers, {
                        orka: {url: "http://127.0.0.1:43210/orka/mcp", http_headers: {}},
                    });
                } else {
                    assert.equal(configReads, 1, "legacy MCP conflict filtering must remain enabled");
                    assert.equal(started.config.mcp_servers, undefined);
                }
                const mode = AgentMode.getInitialAgentMode();
                assert.equal(mode.id, "orka-external");
                assert.equal(mode.approvalPolicy, "on-request");
                assert.deepEqual(mode.sandboxPolicy, {type: "externalSandbox", networkAccess: "restricted"});
            } finally {
                for (const [key, value] of Object.entries(previousEnvironment)) {
                    if (value === undefined) delete process.env[key];
                    else process.env[key] = value;
                }
            }
        });
    }
});

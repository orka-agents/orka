import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

const capability = "integration-test-capability-000000000000000000000000";

test("gateway enforces capability and origin and shuts down", { timeout: 15_000 }, async () => {
  const child = spawn(process.execPath, ["--import", "tsx", "server/index.ts"], {
    cwd: process.cwd(),
    env: {
      ...process.env,
      NODE_ENV: "test",
      ORKA_CONSOLE_NO_OPEN: "1",
      ORKA_CONSOLE_CAPABILITY: capability,
    },
    stdio: ["ignore", "pipe", "pipe"],
  });
  let output = "";
  child.stdout.setEncoding("utf8");
  child.stderr.setEncoding("utf8");
  child.stdout.on("data", (chunk: string) => { output += chunk; });
  child.stderr.on("data", (chunk: string) => { output += chunk; });

  try {
    const port = await waitForPort(() => output);
    const origin = `http://127.0.0.1:${port}`;

    const indexResponse = await fetch(`${origin}/`);
    assert.equal(indexResponse.status, 200);

    const anonymous = await fetch(`${origin}/api/console/health`);
    assert.equal(anonymous.status, 401);

    const authenticated = await fetch(`${origin}/api/console/health`, {
      headers: { "X-Orka-Capability": capability },
    });
    assert.equal(authenticated.status, 200);

    const foreignOrigin = await fetch(`${origin}/api/console/crawl`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Origin: "http://127.0.0.1:1",
        "X-Orka-Capability": capability,
        "X-Orka-Console": "1",
      },
      body: JSON.stringify({ context: "does-not-run" }),
    });
    assert.equal(foreignOrigin.status, 403);
  } finally {
    child.kill("SIGTERM");
    const [code, signal] = await once(child, "exit") as [number | null, NodeJS.Signals | null];
    assert.equal(signal, null, output);
    assert.equal(code, 0, output);
  }
});

test("second shutdown signal kills active detached command groups", { timeout: 15_000 }, async () => {
  const directory = await mkdtemp(join(tmpdir(), "orka-control-shutdown-test-"));
  const pidFile = join(directory, "child.pid");
  const child = spawn(process.execPath, ["--import", "tsx", "server/index.ts"], {
    cwd: process.cwd(),
    env: {
      ...process.env,
      NODE_ENV: "test",
      ORKA_CONSOLE_NO_OPEN: "1",
      ORKA_CONSOLE_CAPABILITY: capability,
      ORKA_CONSOLE_TEST_CHILD_PID_FILE: pidFile,
    },
    stdio: ["ignore", "pipe", "pipe"],
  });
  let output = "";
  child.stdout.setEncoding("utf8");
  child.stderr.setEncoding("utf8");
  child.stdout.on("data", (chunk: string) => { output += chunk; });
  child.stderr.on("data", (chunk: string) => { output += chunk; });

  try {
    await waitForPort(() => output);
    const commandPid = await waitForPid(pidFile);
    child.kill("SIGTERM");
    await new Promise((resolve) => setTimeout(resolve, 100));
    child.kill("SIGTERM");
    await once(child, "exit");
    await waitForProcessExit(commandPid);
  } finally {
    if (child.exitCode === null) child.kill("SIGKILL");
    await rm(directory, { recursive: true, force: true });
  }
});

async function waitForPort(output: () => string): Promise<number> {
  const deadline = Date.now() + 8_000;
  while (Date.now() < deadline) {
    const match = /Orka Control listening on http:\/\/127\.0\.0\.1:(\d+)/.exec(output());
    if (match) return Number(match[1]);
    await new Promise((resolve) => setTimeout(resolve, 25));
  }
  throw new Error(`gateway did not start:\n${output()}`);
}

async function waitForPid(path: string): Promise<number> {
  const deadline = Date.now() + 8_000;
  while (Date.now() < deadline) {
    try { return Number(await readFile(path, "utf8")); }
    catch { await new Promise((resolve) => setTimeout(resolve, 25)); }
  }
  throw new Error("test command did not write its PID");
}

async function waitForProcessExit(pid: number): Promise<void> {
  const deadline = Date.now() + 5_000;
  while (Date.now() < deadline) {
    try { process.kill(pid, 0); }
    catch { return; }
    await new Promise((resolve) => setTimeout(resolve, 25));
  }
  throw new Error(`detached command ${pid} remained alive after forced shutdown`);
}

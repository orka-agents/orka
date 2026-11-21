import assert from "node:assert/strict";
import test from "node:test";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { runCommand } from "./command.js";

test("runCommand waits for termination grace before escalating", async () => {
  const startedAt = Date.now();
  await assert.rejects(
    runCommand(
      process.execPath,
      ["-e", "process.on('SIGTERM', () => {}); setInterval(() => {}, 1000)"],
      { timeoutMs: 250, terminationGraceMs: 180, maxOutputBytes: 1024 },
    ),
    /timed out/,
  );
  assert.ok(Date.now() - startedAt >= 400, "child should receive the configured termination grace");
});

test("runCommand kills descendants when the process-group leader exits on SIGTERM", async () => {
  if (process.platform === "win32") return;
  const directory = await mkdtemp(join(tmpdir(), "orka-control-command-test-"));
  const pidFile = join(directory, "descendant.pid");
  const leader = `
    const { spawn } = require("node:child_process");
    const { writeFileSync } = require("node:fs");
    const child = spawn(process.execPath, ["-e", "process.on('SIGTERM', () => {}); setInterval(() => {}, 1000)"], { stdio: "ignore" });
    writeFileSync(process.argv[1], String(child.pid));
    process.on("SIGTERM", () => process.exit(0));
    setInterval(() => {}, 1000);
  `;

  try {
    await assert.rejects(
      runCommand(process.execPath, ["-e", leader, pidFile], { timeoutMs: 100, terminationGraceMs: 1_000 }),
      /timed out/,
    );
    const descendantPid = Number(await readFile(pidFile, "utf8"));
    await waitForProcessExit(descendantPid);
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});

async function waitForProcessExit(pid: number): Promise<void> {
  const deadline = Date.now() + 3_000;
  while (Date.now() < deadline) {
    try { process.kill(pid, 0); }
    catch { return; }
    await new Promise((resolve) => setTimeout(resolve, 25));
  }
  throw new Error(`descendant process ${pid} remained alive`);
}

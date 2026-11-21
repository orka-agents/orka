import { spawn, type ChildProcess } from "node:child_process";

interface CommandResult {
  stdout: string;
  stderr: string;
}

interface ActiveCommand {
  child: ChildProcess;
  graceMs: number;
  processGroupId?: number;
}

const activeChildren = new Map<ChildProcess, ActiveCommand>();
const activeProcessGroups = new Map<number, number>();
let shuttingDown = false;

export function beginShutdown(): void {
  shuttingDown = true;
}

export function forceTerminateChildren(): void {
  beginShutdown();
  for (const processGroupId of activeProcessGroups.keys()) signalProcessGroup(processGroupId, "SIGKILL");
  for (const command of activeChildren.values()) {
    if (!command.processGroupId) command.child.kill("SIGKILL");
  }
}

export async function terminateChildren(): Promise<void> {
  beginShutdown();
  const startedAt = Date.now();
  const maximumGrace = Math.max(
    0,
    ...activeProcessGroups.values(),
    ...[...activeChildren.values()].map((command) => command.graceMs),
  );
  const deadline = startedAt + maximumGrace + 1_000;

  for (const processGroupId of activeProcessGroups.keys()) signalProcessGroup(processGroupId, "SIGTERM");
  for (const command of activeChildren.values()) {
    if (!command.processGroupId) command.child.kill("SIGTERM");
  }

  while ((activeChildren.size > 0 || activeProcessGroups.size > 0) && Date.now() < deadline) {
    for (const [processGroupId, graceMs] of activeProcessGroups) {
      if (!processGroupExists(processGroupId)) activeProcessGroups.delete(processGroupId);
      else if (Date.now() - startedAt >= graceMs) signalProcessGroup(processGroupId, "SIGKILL");
    }
    for (const command of activeChildren.values()) {
      if (!command.processGroupId && Date.now() - startedAt >= command.graceMs) command.child.kill("SIGKILL");
    }
    await new Promise((resolve) => setTimeout(resolve, 500));
  }

  forceTerminateChildren();
}

export async function runCommand(
  command: string,
  args: string[],
  options: {
    timeoutMs?: number;
    maxOutputBytes?: number;
    input?: string;
    terminationGraceMs?: number;
    processGroup?: boolean;
  } = {},
): Promise<CommandResult> {
  if (shuttingDown) throw new Error("The local gateway is shutting down.");
  const timeoutMs = options.timeoutMs ?? 30_000;
  const maxOutputBytes = options.maxOutputBytes ?? 2_000_000;
  const terminationGraceMs = options.terminationGraceMs ?? 2_000;

  return new Promise((resolve, reject) => {
    const useProcessGroup = process.platform !== "win32" && options.processGroup !== false;
    const child = spawn(command, args, {
      shell: false,
      detached: useProcessGroup,
      env: { ...process.env, NO_COLOR: "1" },
      stdio: [options.input === undefined ? "ignore" : "pipe", "pipe", "pipe"],
    });
    const activeCommand: ActiveCommand = {
      child,
      graceMs: terminationGraceMs,
      processGroupId: useProcessGroup ? child.pid : undefined,
    };
    activeChildren.set(child, activeCommand);
    if (activeCommand.processGroupId) activeProcessGroups.set(activeCommand.processGroupId, terminationGraceMs);

    let stdout = "";
    let stderr = "";
    let outputBytes = 0;
    let settled = false;
    let terminationError: Error | undefined;
    let killTimer: NodeJS.Timeout | undefined;

    const finish = (error?: Error) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      if (killTimer) clearTimeout(killTimer);
      activeChildren.delete(child);
      if (error) reject(error);
      else resolve({ stdout, stderr });
    };
    const signal = (signalName: NodeJS.Signals) => {
      if (activeCommand.processGroupId) signalProcessGroup(activeCommand.processGroupId, signalName);
      else child.kill(signalName);
    };
    const terminate = (error: Error) => {
      if (terminationError || settled) return;
      terminationError = error;
      signal("SIGTERM");
      killTimer = setTimeout(() => signal("SIGKILL"), terminationGraceMs);
    };
    const append = (target: "stdout" | "stderr", chunk: Buffer) => {
      outputBytes += chunk.length;
      if (outputBytes > maxOutputBytes) return terminate(new Error(`${command} returned too much output.`));
      if (target === "stdout") stdout += chunk.toString("utf8");
      else stderr += chunk.toString("utf8");
    };

    child.stdout!.on("data", (chunk: Buffer) => append("stdout", chunk));
    child.stderr!.on("data", (chunk: Buffer) => append("stderr", chunk));
    child.on("error", finish);
    child.on("close", (code, closeSignal) => {
      if (activeCommand.processGroupId && !shuttingDown) {
        // A leader can exit while descendants remain. Kill the group before
        // releasing the only tracked PGID during normal operation.
        signalProcessGroup(activeCommand.processGroupId, "SIGKILL");
        activeProcessGroups.delete(activeCommand.processGroupId);
      } else if (activeCommand.processGroupId && !processGroupExists(activeCommand.processGroupId)) {
        // During gateway shutdown the group stays tracked after leader exit so
        // terminateChildren or a second signal can still kill descendants.
        activeProcessGroups.delete(activeCommand.processGroupId);
      }

      if (terminationError) finish(terminationError);
      else if (code === 0) finish();
      else finish(new Error(commandError(command, stderr, code, closeSignal)));
    });

    const timer = setTimeout(
      () => terminate(new Error(`${command} timed out after ${Math.round(timeoutMs / 1_000)} seconds.`)),
      timeoutMs,
    );
    if (options.input !== undefined) child.stdin!.end(options.input);
  });
}

function signalProcessGroup(processGroupId: number, signal: NodeJS.Signals): void {
  try { process.kill(-processGroupId, signal); }
  catch { /* Process group already exited. */ }
}

function processGroupExists(processGroupId: number): boolean {
  try {
    process.kill(-processGroupId, 0);
    return true;
  } catch {
    return false;
  }
}

function commandError(command: string, stderr: string, code: number | null, signal: NodeJS.Signals | null): string {
  const detail = stderr
    .replace(/Bearer\s+\S+/gi, "Bearer [redacted]")
    .replace(/token[=:]\s*\S+/gi, "token=[redacted]")
    .trim()
    .slice(0, 800);
  const exit = signal ? `signal ${signal}` : `exit ${code ?? "unknown"}`;
  return `${command} failed (${exit})${detail ? `: ${detail}` : "."}`;
}

export async function runJson<T>(command: string, args: string[], timeoutMs = 30_000, input?: string): Promise<T> {
  const result = await runCommand(command, args, { timeoutMs, input });
  try { return JSON.parse(result.stdout) as T; }
  catch { throw new Error(`${command} returned an invalid response.`); }
}

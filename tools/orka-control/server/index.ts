import { createServer, type IncomingMessage, type ServerResponse } from "node:http";
import { randomBytes, timingSafeEqual } from "node:crypto";
import { chmod, mkdir, readFile, realpath, rm, stat, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { extname, join, normalize, relative } from "node:path";
import { z } from "zod";
import { crawlCluster, createTask, listContexts } from "./cluster.js";
import { beginShutdown, forceTerminateChildren, runCommand, terminateChildren } from "./command.js";
import { createInstallPlan, installOrka } from "./install.js";

const host = "127.0.0.1";
const requestedPort = Number(process.env.ORKA_CONSOLE_PORT ?? "0");
let port = requestedPort;
const dist = join(process.cwd(), "dist");
const testMode = process.env.NODE_ENV === "test" && process.env.ORKA_CONSOLE_NO_OPEN === "1";
const configuredCapability = testMode ? process.env.ORKA_CONSOLE_CAPABILITY : undefined;
if (configuredCapability && configuredCapability.length < 32) throw new Error("Test capability must contain at least 32 characters.");
const capability = configuredCapability ?? randomBytes(32).toString("base64url");
const requestBuckets = new Map<string, number[]>();
let activeApiRequests = 0;

const dnsLabel = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/;
const crawlSchema = z.object({ context: z.string().min(1).max(253), namespace: z.string().regex(dnsLabel).optional() }).strict();
const runSchema = z.object({
  idempotencyKey: z.string().uuid(),
  context: z.string().min(1).max(253),
  namespace: z.string().regex(dnsLabel),
  mode: z.enum(["smoke", "agent"]),
  agentName: z.string().regex(dnsLabel).optional(),
  prompt: z.string().trim().min(1).max(8_000).optional(),
}).strict().superRefine((value, context) => {
  if (value.mode === "agent" && (!value.agentName || !value.prompt)) {
    context.addIssue({ code: "custom", message: "Agent runs require an existing agent and prompt." });
  }
});
const planSchema = z.object({ context: z.string().min(1).max(253) }).strict();
const installSchema = z.object({
  planId: z.string().min(20).max(64),
  context: z.string().min(1).max(253),
  confirmation: z.string().min(1).max(253),
  acknowledgeClusterAccess: z.literal(true),
  acknowledgePersistence: z.literal(true),
}).strict();

class HttpError extends Error {
  constructor(readonly status: number, message: string) { super(message); }
}

function sendJson(response: ServerResponse, status: number, payload: unknown) {
  response.writeHead(status, {
    "Content-Type": "application/json; charset=utf-8",
    "Cache-Control": "no-store",
    "X-Content-Type-Options": "nosniff",
    "X-Frame-Options": "DENY",
    "Referrer-Policy": "no-referrer",
    "Content-Security-Policy": "default-src 'none'; frame-ancestors 'none'",
  });
  response.end(JSON.stringify(payload));
}

async function readBody(request: IncomingMessage): Promise<unknown> {
  const contentType = request.headers["content-type"] ?? "";
  if (!contentType.startsWith("application/json")) throw new HttpError(415, "Requests must use application/json.");
  let bytes = 0;
  const chunks: Buffer[] = [];
  for await (const chunkValue of request) {
    const chunk = Buffer.isBuffer(chunkValue) ? chunkValue : Buffer.from(chunkValue);
    bytes += chunk.length;
    if (bytes > 32_768) throw new HttpError(413, "Request body is too large.");
    chunks.push(chunk);
  }
  try { return JSON.parse(Buffer.concat(chunks).toString("utf8")); }
  catch { throw new HttpError(400, "Request body is not valid JSON."); }
}

function checkHost(request: IncomingMessage) {
  if (request.headers.host !== `127.0.0.1:${port}`) throw new HttpError(403, "Invalid gateway host.");
}

function checkCapability(request: IncomingMessage) {
  const supplied = request.headers["x-orka-capability"];
  if (typeof supplied !== "string") throw new HttpError(401, "Console session authentication is required.");
  const expectedBuffer = Buffer.from(capability);
  const suppliedBuffer = Buffer.from(supplied);
  if (expectedBuffer.length !== suppliedBuffer.length || !timingSafeEqual(expectedBuffer, suppliedBuffer)) {
    throw new HttpError(401, "Console session authentication is invalid.");
  }
}

function checkMutationRequest(request: IncomingMessage) {
  if (request.headers["x-orka-console"] !== "1") throw new HttpError(403, "Missing console request marker.");
  if (request.headers.origin !== `http://127.0.0.1:${port}`) throw new HttpError(403, "Cross-origin requests are not allowed.");
}

function checkRateLimit(pathname: string) {
  const routes = new Set(["/api/console/health", "/api/console/contexts", "/api/console/crawl", "/api/console/tasks", "/api/console/install/plan", "/api/console/install"]);
  const key = routes.has(pathname) ? pathname : "unknown";
  const now = Date.now();
  const limit = key === "/api/console/install" ? 2 : key === "/api/console/tasks" || key === "/api/console/install/plan" ? 10 : 30;
  const recent = (requestBuckets.get(key) ?? []).filter((timestamp) => now - timestamp < 60_000);
  if (recent.length >= limit) throw new HttpError(429, "Too many requests. Wait before trying again.");
  recent.push(now);
  requestBuckets.set(key, recent);
}

async function handleApi(request: IncomingMessage, response: ServerResponse, url: URL) {
  checkCapability(request);
  checkRateLimit(url.pathname);
  if (request.method === "GET" && url.pathname === "/api/console/health") return sendJson(response, 200, { ok: true });
  if (request.method === "GET" && url.pathname === "/api/console/contexts") return sendJson(response, 200, { items: await listContexts() });
  if (request.method === "POST") checkMutationRequest(request);
  if (request.method === "POST" && url.pathname === "/api/console/crawl") {
    const input = crawlSchema.parse(await readBody(request));
    return sendJson(response, 200, await crawlCluster(input.context, input.namespace));
  }
  if (request.method === "POST" && url.pathname === "/api/console/tasks") {
    const input = runSchema.parse(await readBody(request));
    return sendJson(response, 201, await createTask(input.context, input.namespace, input));
  }
  if (request.method === "POST" && url.pathname === "/api/console/install/plan") {
    const input = planSchema.parse(await readBody(request));
    return sendJson(response, 200, await createInstallPlan(input.context));
  }
  if (request.method === "POST" && url.pathname === "/api/console/install") {
    const input = installSchema.parse(await readBody(request));
    return sendJson(response, 200, await installOrka(input));
  }
  throw new HttpError(404, "Console endpoint not found.");
}

const contentTypes: Record<string, string> = {
  ".html": "text/html; charset=utf-8", ".js": "text/javascript; charset=utf-8", ".css": "text/css; charset=utf-8",
  ".svg": "image/svg+xml", ".json": "application/json; charset=utf-8", ".woff": "font/woff", ".woff2": "font/woff2",
};

async function serveStatic(response: ServerResponse, pathname: string) {
  const relativePath = pathname === "/" ? "index.html" : normalize(pathname).replace(/^[/\\]+/, "");
  let filePath = join(dist, relativePath);
  if (!filePath.startsWith(dist)) throw new HttpError(404, "Not found.");
  try {
    if (!(await stat(filePath)).isFile()) throw new Error("not file");
  } catch {
    filePath = join(dist, "index.html");
  }
  const [realDist, realFile] = await Promise.all([realpath(dist), realpath(filePath)]);
  const containment = relative(realDist, realFile);
  if (containment.startsWith("..") || containment.includes(`..${process.platform === "win32" ? "\\" : "/"}`)) {
    throw new HttpError(404, "Not found.");
  }
  response.writeHead(200, {
    "Content-Type": contentTypes[extname(realFile)] ?? "application/octet-stream",
    "X-Content-Type-Options": "nosniff",
    "X-Frame-Options": "DENY",
    "Referrer-Policy": "no-referrer",
    "Content-Security-Policy": "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; font-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'",
  });
  response.end(await readFile(realFile));
}

const server = createServer(async (request, response) => {
  try {
    checkHost(request);
    const url = new URL(request.url ?? "/", `http://${request.headers.host}`);
    if (url.pathname.startsWith("/api/console/")) {
      if (activeApiRequests >= 8) throw new HttpError(503, "The local gateway is busy. Try again shortly.");
      activeApiRequests += 1;
      try { await handleApi(request, response, url); }
      finally { activeApiRequests -= 1; }
    } else await serveStatic(response, url.pathname);
  } catch (error) {
    const status = error instanceof HttpError ? error.status : error instanceof z.ZodError ? 400 : 500;
    const correlationId = randomBytes(6).toString("hex");
    if (status === 500) console.error(`[${correlationId}]`, error);
    const message = error instanceof z.ZodError
      ? error.issues[0]?.message ?? "Invalid request."
      : error instanceof HttpError ? error.message : `Cluster operation failed. See gateway log ${correlationId}.`;
    sendJson(response, status, { message });
  }
});

async function openAuthenticatedConsole() {
  if (process.env.ORKA_CONSOLE_NO_OPEN === "1") return;
  const directory = join(tmpdir(), `orka-control-${process.getuid?.() ?? "user"}`);
  const launcher = join(directory, "launch.html");
  await mkdir(directory, { recursive: true, mode: 0o700 });
  await chmod(directory, 0o700);
  const target = `http://127.0.0.1:${port}/#cap=${capability}`;
  const html = `<!doctype html><meta charset="utf-8"><meta name="referrer" content="no-referrer"><title>Opening Orka Control</title><script>location.replace(${JSON.stringify(target)})</script>`;
  await writeFile(launcher, html, { mode: 0o600 });
  const opener = process.platform === "darwin"
    ? ["open", [launcher]] as const
    : process.platform === "win32"
      ? ["explorer.exe", [launcher]] as const
      : ["xdg-open", [launcher]] as const;
  const opened = await runCommand(opener[0], [...opener[1]], { timeoutMs: 10_000, maxOutputBytes: 10_000, processGroup: false })
    .then(() => true)
    .catch(() => false);
  if (!opened) {
    console.error(`Could not open a browser. Open this owner-only launcher instead: ${launcher}`);
    return;
  }
  setTimeout(() => void rm(launcher, { force: true }), 60_000).unref();
}

server.listen(requestedPort, host, () => {
  const address = server.address();
  if (!address || typeof address === "string") throw new Error("Could not determine the local gateway port.");
  port = address.port;
  console.log(`Orka Control listening on http://${host}:${port}`);
  if (testMode && process.env.ORKA_CONSOLE_TEST_CHILD_PID_FILE) {
    const pidFile = process.env.ORKA_CONSOLE_TEST_CHILD_PID_FILE;
    void runCommand(
      process.execPath,
      ["-e", "require('node:fs').writeFileSync(process.argv[1], String(process.pid)); process.on('SIGTERM', () => {}); setInterval(() => {}, 1000)", pidFile],
      { timeoutMs: 10 * 60_000, terminationGraceMs: 10 * 60_000, maxOutputBytes: 1_024 },
    ).catch(() => undefined);
  }
  void openAuthenticatedConsole();
});

let shuttingDown = false;
async function shutdown() {
  if (shuttingDown) return;
  shuttingDown = true;
  beginShutdown();
  const forceCloseTimer = setTimeout(() => server.closeAllConnections(), 5_000);
  const serverClosed = new Promise<void>((resolve) => server.close(() => resolve())).finally(() => clearTimeout(forceCloseTimer));
  await Promise.all([serverClosed, terminateChildren()]);
  process.exitCode = 0;
}
function handleSignal() {
  if (shuttingDown) {
    forceTerminateChildren();
    server.closeAllConnections();
    process.exit(1);
  }
  void shutdown();
}
process.on("SIGINT", handleSignal);
process.on("SIGTERM", handleSignal);

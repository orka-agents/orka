import { createHash } from "node:crypto";
import type {
  ClusterReport,
  ComponentHealth,
  KubeContext,
  OrkaAgent,
  OrkaTask,
  PermissionCheck,
  TaskPhase,
} from "../src/domain.js";
import { runCommand, runJson } from "./command.js";
import { release } from "./release.js";

interface KubeConfigJson {
  "current-context"?: string;
  contexts?: Array<{ name: string; context: { cluster: string; namespace?: string } }>;
  clusters?: Array<{ name: string; cluster: { server: string } }>;
}

interface KubeList<T> { items: T[] }
interface Metadata { name: string; namespace?: string; uid?: string; creationTimestamp?: string; labels?: Record<string, string>; annotations?: Record<string, string> }
interface Unstructured { metadata: Metadata; spec?: Record<string, unknown>; status?: Record<string, unknown> }
export interface AuthorizationRule { verbs: string[]; apiGroups: string[]; resources: string[]; resourceNames?: string[] }
interface RulesReview { status?: { resourceRules?: AuthorizationRule[]; incomplete?: boolean; evaluationError?: string } }
interface UserInfo { username?: string; uid?: string; groups?: string[]; extra?: Record<string, string[]> }
interface SelfSubjectReview { status?: { userInfo?: UserInfo } }
interface AccessReview { status?: { allowed?: boolean; evaluationError?: string } }

const idempotencyKeyAnnotation = "control.orka.ai/idempotency-key";
const payloadHashAnnotation = "control.orka.ai/payload-sha256";

const taskRequests = new Map<string, { expiresAt: number; payloadHash: string; promise: Promise<OrkaTask> }>();
const taskLocks = new Map<string, Promise<void>>();

async function withTaskLock<T>(key: string, operation: () => Promise<T>): Promise<T> {
  const previous = taskLocks.get(key) ?? Promise.resolve();
  let releaseLock: () => void = () => {};
  const current = new Promise<void>((resolve) => { releaseLock = resolve; });
  const tail = previous.then(() => current);
  taskLocks.set(key, tail);
  await previous;
  try { return await operation(); }
  finally {
    releaseLock();
    if (taskLocks.get(key) === tail) taskLocks.delete(key);
  }
}

function kubectlArgs(context: string, args: string[]): string[] {
  return ["--context", context, "--request-timeout=20s", ...args];
}

export async function listContexts(): Promise<KubeContext[]> {
  const config = await runJson<KubeConfigJson>("kubectl", ["config", "view", "-o", "json"]);
  const clusters = new Map((config.clusters ?? []).map((cluster) => [cluster.name, cluster.cluster.server]));
  return (config.contexts ?? []).map((entry) => ({
    name: entry.name,
    cluster: entry.context.cluster,
    server: sanitizeServer(clusters.get(entry.context.cluster) ?? "unknown"),
    namespace: entry.context.namespace ?? "default",
    current: entry.name === config["current-context"],
  }));
}

export function sanitizeServer(server: string): string {
  try {
    const url = new URL(server);
    return url.origin;
  } catch {
    return "unknown";
  }
}

export async function requireContext(name: string): Promise<KubeContext> {
  const context = (await listContexts()).find((candidate) => candidate.name === name);
  if (!context) throw new Error("The selected Kubernetes context no longer exists.");
  return context;
}

async function optionalJson<T>(context: string, args: string[]): Promise<T | undefined> {
  try { return await runJson<T>("kubectl", kubectlArgs(context, args)); }
  catch { return undefined; }
}

async function canI(context: string, verb: string, resource: string, namespace?: string): Promise<boolean> {
  const args = ["auth", "can-i", verb, resource];
  if (namespace) args.push("--namespace", namespace);
  try {
    const result = await runCommand("kubectl", kubectlArgs(context, args));
    return result.stdout.trim() === "yes";
  } catch { return false; }
}

export async function subjectRules(context: string, namespace: string): Promise<AuthorizationRule[]> {
  const body = JSON.stringify({
    apiVersion: "authorization.k8s.io/v1",
    kind: "SelfSubjectRulesReview",
    spec: { namespace },
  });
  const review = await runJson<RulesReview>(
    "kubectl",
    kubectlArgs(context, ["create", "--raw", "/apis/authorization.k8s.io/v1/selfsubjectrulesreviews", "-f", "-"]),
    30_000,
    body,
  );
  if (!review.status || review.status.incomplete || review.status.evaluationError) {
    throw new Error(review.status?.evaluationError || `Kubernetes returned an incomplete authorization review for ${namespace}.`);
  }
  return review.status.resourceRules ?? [];
}

export function rulesAllow(rules: AuthorizationRule[], verb: string, resource: string, resourceName?: string): boolean {
  const separator = resource.indexOf(".");
  const resourcePart = separator === -1 ? resource : resource.slice(0, separator);
  const apiGroup = separator === -1 ? "" : resource.slice(separator + 1);
  return rules.some((rule) => {
    if (!rule.verbs.includes("*") && !rule.verbs.includes(verb)) return false;
    if (!rule.apiGroups.includes("*") && !rule.apiGroups.includes(apiGroup)) return false;
    if (!rule.resources.includes("*") && !rule.resources.includes(resourcePart)) return false;
    if (rule.resourceNames?.length && (!resourceName || !rule.resourceNames.includes(resourceName))) return false;
    return true;
  });
}

export async function accessAllowed(
  context: string,
  requirement: { verb: string; resource: string; resourceName?: string; namespace?: string },
): Promise<boolean> {
  const separator = requirement.resource.indexOf(".");
  const resource = separator === -1 ? requirement.resource : requirement.resource.slice(0, separator);
  const group = separator === -1 ? "" : requirement.resource.slice(separator + 1);
  const body = JSON.stringify({
    apiVersion: "authorization.k8s.io/v1",
    kind: "SelfSubjectAccessReview",
    spec: {
      resourceAttributes: {
        group,
        resource,
        verb: requirement.verb,
        ...(requirement.resourceName ? { name: requirement.resourceName } : {}),
        ...(requirement.namespace ? { namespace: requirement.namespace } : {}),
      },
    },
  });
  const review = await runJson<AccessReview>(
    "kubectl",
    kubectlArgs(context, ["create", "--raw", "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews", "-f", "-"]),
    30_000,
    body,
  );
  return review.status?.allowed === true && !review.status.evaluationError;
}

function text(value: unknown): string | undefined {
  return typeof value === "string" && value ? value : undefined;
}

function numberValue(value: unknown): number {
  return typeof value === "number" && Number.isFinite(value) ? value : 0;
}

function nested(object: Record<string, unknown> | undefined, ...keys: string[]): unknown {
  let value: unknown = object;
  for (const key of keys) {
    if (!value || typeof value !== "object" || Array.isArray(value)) return undefined;
    value = (value as Record<string, unknown>)[key];
  }
  return value;
}

export function normalizeTask(resource: Unstructured): OrkaTask {
  const phaseValue = text(resource.status?.phase) ?? "Unknown";
  const phases = new Set<TaskPhase>(["Pending", "Running", "Finalizing", "Succeeded", "Failed", "Scheduled", "Cancelled", "Unknown"]);
  const phase = phases.has(phaseValue as TaskPhase) ? phaseValue as TaskPhase : "Unknown";
  const typeValue = text(resource.spec?.type);
  const type = typeValue === "container" || typeValue === "ai" || typeValue === "agent" ? typeValue : "unknown";
  return {
    uid: resource.metadata.uid ?? `${resource.metadata.namespace}/${resource.metadata.name}`,
    name: resource.metadata.name,
    namespace: resource.metadata.namespace ?? "orka-system",
    type,
    phase,
    agentName: text(nested(resource.spec, "agentRef", "name")),
    createdAt: resource.metadata.creationTimestamp ?? new Date(0).toISOString(),
    startedAt: text(resource.status?.startTime),
    completedAt: text(resource.status?.completionTime),
    attempts: numberValue(resource.status?.attempts),
  };
}

function normalizeAgent(resource: Unstructured): OrkaAgent {
  return {
    uid: resource.metadata.uid ?? `${resource.metadata.namespace}/${resource.metadata.name}`,
    name: resource.metadata.name,
    namespace: resource.metadata.namespace ?? "orka-system",
    ready: resource.status?.ready === true,
    runtime: text(nested(resource.spec, "runtime", "type")) ?? text(nested(resource.spec, "runtime", "runtimeRef", "name")) ?? "native",
    model: text(nested(resource.spec, "model", "name")) ?? text(resource.spec?.model) ?? "default",
    activeTasks: numberValue(resource.status?.activeTasks),
    lastUsed: text(resource.status?.lastUsed),
  };
}

function deploymentComponent(resource: Unstructured): ComponentHealth {
  const desired = numberValue(resource.spec?.replicas);
  const ready = numberValue(resource.status?.readyReplicas);
  return { name: resource.metadata.name, kind: "Deployment", ready: desired > 0 && ready >= desired, detail: `${ready}/${desired} replicas ready` };
}

function serviceComponent(resource: Unstructured): ComponentHealth {
  return { name: resource.metadata.name, kind: "Service", ready: true, detail: "ClusterIP API service" };
}

function pvcComponent(resource: Unstructured): ComponentHealth {
  const phase = text(resource.status?.phase) ?? "Unknown";
  return { name: resource.metadata.name, kind: "PersistentVolumeClaim", ready: phase === "Bound", detail: phase };
}

export function selectControllerDeployment(resources: Unstructured[]): Unstructured | undefined {
  return resources.find((item) => item.metadata.labels?.["app.kubernetes.io/component"] === "controller")
    ?? resources.find((item) => item.metadata.labels?.["control-plane"] === "controller-manager");
}

export async function crawlCluster(contextName: string, namespaceHint?: string): Promise<ClusterReport> {
  const context = await requireContext(contextName);
  const version = await runJson<{ serverVersion?: { gitVersion?: string } }>("kubectl", kubectlArgs(contextName, ["version", "-o", "json"]));
  const crds = await optionalJson<KubeList<Unstructured>>(contextName, ["get", "crd", "-o", "json"]);
  const foundCrds = release.crds.filter((name) => crds?.items.some((item) => item.metadata.name === name));
  const allDeployments = await optionalJson<KubeList<Unstructured>>(contextName, ["get", "deployments", "--all-namespaces", "-l", "app.kubernetes.io/name=orka", "-o", "json"]);
  const allServices = await optionalJson<KubeList<Unstructured>>(contextName, ["get", "services", "--all-namespaces", "-l", "app.kubernetes.io/name=orka", "-o", "json"]);
  const controller = selectControllerDeployment(allDeployments?.items ?? []);
  const namespace = namespaceHint || controller?.metadata.namespace || allServices?.items[0]?.metadata.namespace || "orka-system";
  const releaseName = controller?.metadata.labels?.["app.kubernetes.io/instance"] ?? allServices?.items[0]?.metadata.labels?.["app.kubernetes.io/instance"];
  const versionLabel = controller?.metadata.labels?.["app.kubernetes.io/version"];

  const [nodes, storageClasses, tasks, agents, providers, monitors, scans, pools, deployments, services, pvcs] = await Promise.all([
    optionalJson<KubeList<Unstructured>>(contextName, ["get", "nodes", "-o", "json"]),
    optionalJson<KubeList<Unstructured>>(contextName, ["get", "storageclass", "-o", "json"]),
    foundCrds.includes("tasks.core.orka.ai") ? optionalJson<KubeList<Unstructured>>(contextName, ["get", "tasks.core.orka.ai", "-n", namespace, "-o", "json"]) : undefined,
    foundCrds.includes("agents.core.orka.ai") ? optionalJson<KubeList<Unstructured>>(contextName, ["get", "agents.core.orka.ai", "-n", namespace, "-o", "json"]) : undefined,
    foundCrds.includes("providers.core.orka.ai") ? optionalJson<KubeList<Unstructured>>(contextName, ["get", "providers.core.orka.ai", "-n", namespace, "-o", "json"]) : undefined,
    foundCrds.includes("repositorymonitors.core.orka.ai") ? optionalJson<KubeList<Unstructured>>(contextName, ["get", "repositorymonitors.core.orka.ai", "-n", namespace, "-o", "json"]) : undefined,
    foundCrds.includes("repositoryscans.core.orka.ai") ? optionalJson<KubeList<Unstructured>>(contextName, ["get", "repositoryscans.core.orka.ai", "-n", namespace, "-o", "json"]) : undefined,
    optionalJson<KubeList<Unstructured>>(contextName, ["get", "runtimepools.core.orka.ai", "-n", namespace, "-o", "json"]),
    optionalJson<KubeList<Unstructured>>(contextName, ["get", "deployments", "-n", namespace, "-l", "app.kubernetes.io/name=orka", "-o", "json"]),
    optionalJson<KubeList<Unstructured>>(contextName, ["get", "services", "-n", namespace, "-l", "app.kubernetes.io/name=orka", "-o", "json"]),
    optionalJson<KubeList<Unstructured>>(contextName, ["get", "pvc", "-n", namespace, "-l", "app.kubernetes.io/name=orka", "-o", "json"]),
  ]);

  const defaultStorageClass = storageClasses?.items.find((item) => item.metadata.annotations?.["storageclass.kubernetes.io/is-default-class"] === "true")?.metadata.name;
  const controllerResource = selectControllerDeployment(deployments?.items ?? []);
  const controllerReady = controllerResource ? deploymentComponent(controllerResource).ready : false;
  const installed = Boolean(controllerResource || services?.items.length);
  const partial = foundCrds.length > 0 && !installed;
  const state = controllerReady ? "ready" : installed ? "degraded" : partial ? "partial" : "absent";
  const permissions: PermissionCheck[] = await Promise.all([
    ["customresourcedefinitions.apiextensions.k8s.io", "get"],
    ["tasks.core.orka.ai", "list"],
    ["tasks.core.orka.ai", "create"],
    ["agents.core.orka.ai", "list"],
  ].map(async ([resource, verb]) => ({ resource, verb, allowed: await canI(contextName, verb, resource, resource.includes("core.orka.ai") ? namespace : undefined) })));
  const components: ComponentHealth[] = [
    ...(deployments?.items ?? []).map(deploymentComponent),
    ...(services?.items ?? []).map(serviceComponent),
    ...(pvcs?.items ?? []).map(pvcComponent),
  ];
  const warnings: string[] = [];
  if (!defaultStorageClass) warnings.push("No default StorageClass was detected; Orka persistence cannot be provisioned automatically.");
  if (partial) warnings.push("Orka CRDs exist without a live controller. Quick Deploy is blocked to avoid an incompatible reinstall.");
  if (installed && !controllerReady) warnings.push("An Orka release was found, but its controller is not fully ready.");

  return {
    context,
    crawledAt: new Date().toISOString(),
    kubernetesVersion: version.serverVersion?.gitVersion ?? "unknown",
    nodeCount: nodes?.items.length ?? 0,
    defaultStorageClass,
    orka: {
      state,
      installed,
      namespace,
      releaseName,
      version: versionLabel,
      crdCount: foundCrds.length,
      expectedCrdCount: release.crds.length,
      controllerReady,
      message: controllerReady
        ? "Orka is installed and the controller is ready."
        : installed
          ? "Orka is installed, but the controller needs attention."
          : partial
            ? "Orka CRDs remain, but no controller was discovered."
            : "No Orka installation was detected on this cluster.",
    },
    counts: {
      tasks: tasks?.items.length ?? 0,
      agents: agents?.items.length ?? 0,
      providers: providers?.items.length ?? 0,
      monitors: (monitors?.items.length ?? 0) + (scans?.items.length ?? 0),
      runtimePools: pools?.items.length ?? 0,
    },
    tasks: (tasks?.items ?? []).map(normalizeTask).sort((a, b) => b.createdAt.localeCompare(a.createdAt)),
    agents: (agents?.items ?? []).map(normalizeAgent).sort((a, b) => a.name.localeCompare(b.name)),
    components,
    permissions,
    warnings,
  };
}

export function taskCountsAgainstQuota(task: OrkaTask): boolean {
  return !["Succeeded", "Failed", "Cancelled", "Scheduled"].includes(task.phase);
}

export function canonicalIdempotencyKey(idempotencyKey: string): string {
  return idempotencyKey.toLowerCase();
}

export function taskNameForRequest(idempotencyKey: string): string {
  return `console-${canonicalIdempotencyKey(idempotencyKey)}`;
}

function taskPayloadHash(input: { mode: "smoke" | "agent"; agentName?: string; prompt?: string }): string {
  return createHash("sha256").update(JSON.stringify({ mode: input.mode, agentName: input.agentName, prompt: input.prompt })).digest("hex");
}

function verifyIdempotentTask(resource: Unstructured, idempotencyKey: string, payloadHash: string): OrkaTask {
  const annotations = resource.metadata.annotations ?? {};
  if (annotations[idempotencyKeyAnnotation] !== idempotencyKey || annotations[payloadHashAnnotation] !== payloadHash) {
    throw new Error("The deterministic Task name already exists with different input.");
  }
  return normalizeTask(resource);
}

async function getTask(context: string, namespace: string, name: string): Promise<Unstructured | undefined> {
  return optionalJson<Unstructured>(context, ["get", "tasks.core.orka.ai", name, "-n", namespace, "-o", "json"]);
}

export async function createTask(
  context: string,
  namespace: string,
  input: { idempotencyKey: string; mode: "smoke" | "agent"; agentName?: string; prompt?: string },
): Promise<OrkaTask> {
  const now = Date.now();
  for (const [key, request] of taskRequests) if (request.expiresAt <= now) taskRequests.delete(key);
  const canonicalKey = canonicalIdempotencyKey(input.idempotencyKey);
  const requestKey = `${context}\n${namespace}\n${canonicalKey}`;
  const payloadHash = taskPayloadHash(input);
  const existing = taskRequests.get(requestKey);
  if (existing) {
    if (existing.payloadHash !== payloadHash) throw new Error("This idempotency key was already used for different Task input.");
    return existing.promise;
  }
  if (taskRequests.size >= 128) taskRequests.delete(taskRequests.keys().next().value as string);
  const promise = withTaskLock(`${context}\n${namespace}`, () => createTaskOnce(context, namespace, { ...input, idempotencyKey: canonicalKey })).catch((error) => {
    taskRequests.delete(requestKey);
    throw error;
  });
  taskRequests.set(requestKey, { expiresAt: now + 10 * 60_000, payloadHash, promise });
  return promise;
}

async function createTaskOnce(
  context: string,
  namespace: string,
  input: { idempotencyKey: string; mode: "smoke" | "agent"; agentName?: string; prompt?: string },
): Promise<OrkaTask> {
  const payloadHash = taskPayloadHash(input);
  const name = taskNameForRequest(input.idempotencyKey);
  const existing = await getTask(context, namespace, name);
  if (existing) return verifyIdempotentTask(existing, input.idempotencyKey, payloadHash);

  const report = await crawlCluster(context, namespace);
  if (report.orka.state !== "ready") throw new Error("Orka must be ready before a Task can run.");
  const [canList, canCreate] = await Promise.all([
    canI(context, "list", "tasks.core.orka.ai", namespace),
    canI(context, "create", "tasks.core.orka.ai", namespace),
  ]);
  if (!canList) throw new Error("The current Kubernetes identity cannot list Orka Tasks, so quota cannot be verified.");
  if (!canCreate) throw new Error("The current Kubernetes identity cannot create Orka Tasks in this namespace.");
  const taskList = await runJson<KubeList<Unstructured>>("kubectl", kubectlArgs(context, ["get", "tasks.core.orka.ai", "-n", namespace, "-o", "json"]));
  if (taskList.items.map(normalizeTask).filter(taskCountsAgainstQuota).length >= 5) {
    throw new Error("This console limits each namespace to five active Tasks.");
  }
  if (input.mode === "agent") {
    const agentList = await runJson<KubeList<Unstructured>>("kubectl", kubectlArgs(context, ["get", "agents.core.orka.ai", "-n", namespace, "-o", "json"]));
    const agent = agentList.items.map(normalizeAgent).find((candidate) => candidate.name === input.agentName);
    if (!agent) throw new Error("The selected Agent no longer exists in this namespace.");
    if (!agent.ready) throw new Error("The selected Agent is not ready.");
  }

  const spec = input.mode === "smoke"
    ? { type: "container", command: ["/bin/sh", "-c", "printf ORKA_CONSOLE_OK"], timeout: "3m", retryPolicy: { maxRetries: 0 } }
    : { type: "agent", agentRef: { name: input.agentName }, prompt: input.prompt, timeout: "15m", priority: 100 };
  const body = JSON.stringify({
    apiVersion: "core.orka.ai/v1alpha1",
    kind: "Task",
    metadata: {
      name,
      namespace,
      labels: { "app.kubernetes.io/created-by": "orka-control" },
      annotations: {
        [idempotencyKeyAnnotation]: input.idempotencyKey,
        [payloadHashAnnotation]: payloadHash,
      },
    },
    spec,
  });
  try {
    return normalizeTask(await runJson<Unstructured>("kubectl", kubectlArgs(context, ["create", "-f", "-", "-o", "json"]), 30_000, body));
  } catch (createError) {
    const reconciled = await getTask(context, namespace, name);
    if (reconciled) return verifyIdempotentTask(reconciled, input.idempotencyKey, payloadHash);
    throw createError;
  }
}

export function stableUserInfo(review: SelfSubjectReview): UserInfo {
  const userInfo = review.status?.userInfo;
  if (!userInfo?.username) throw new Error("Kubernetes did not return a stable caller identity.");
  const extra = Object.fromEntries(Object.entries(userInfo.extra ?? {})
    .sort(([left], [right]) => left.localeCompare(right))
    .map(([key, values]) => [key, [...values].sort()]));
  return {
    username: userInfo.username,
    uid: userInfo.uid ?? "",
    groups: [...(userInfo.groups ?? [])].sort(),
    extra,
  };
}

export async function clusterFingerprint(contextName: string): Promise<string> {
  const context = await requireContext(contextName);
  const certificate = await runCommand("kubectl", [
    "--context", contextName, "config", "view", "--raw", "--minify", "-o",
    "jsonpath={.clusters[0].cluster.certificate-authority-data}",
  ]).then((result) => result.stdout).catch(() => "");
  const [clusterUid, identityReview] = await Promise.all([
    runCommand("kubectl", kubectlArgs(contextName, ["get", "namespace", "kube-system", "-o", "jsonpath={.metadata.uid}"])).then((result) => result.stdout),
    runJson<SelfSubjectReview>("kubectl", kubectlArgs(contextName, ["auth", "whoami", "-o", "json"])),
  ]);
  const identity = JSON.stringify(stableUserInfo(identityReview));
  return createHash("sha256").update(`${context.name}\n${context.cluster}\n${context.server}\n${clusterUid}\n${identity}\n${certificate}`).digest("hex").slice(0, 16);
}

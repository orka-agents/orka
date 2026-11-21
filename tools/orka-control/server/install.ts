import { createHash, randomBytes } from "node:crypto";
import { chmod, mkdtemp, open, readFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { parseAllDocuments } from "yaml";
import type { ClusterReport, InstallPlan } from "../src/domain.js";
import { accessAllowed, clusterFingerprint, crawlCluster, requireContext, rulesAllow, subjectRules } from "./cluster.js";
import { runCommand } from "./command.js";
import { helmImageArgs, release } from "./release.js";

const PLAN_TTL_MS = 5 * 60_000;
const MAX_PLANS = 32;
const plans = new Map<string, InstallPlan>();
let installInProgress = false;
let processChartDirectory: Promise<string> | undefined;

interface ChartResource {
  kind?: string;
  metadata?: { name?: string; namespace?: string };
  roleRef?: { apiGroup?: string; kind?: string; name?: string };
}

export interface RenderedResource {
  kind: string;
  resource: string;
  name: string;
  namespace?: string;
  roleRef?: { resource: string; name: string; namespace?: string };
}

const resourceNames = new Map<string, { resource: string; namespaced: boolean }>([
  ["ServiceAccount", { resource: "serviceaccounts", namespaced: true }],
  ["Secret", { resource: "secrets", namespaced: true }],
  ["ConfigMap", { resource: "configmaps", namespaced: true }],
  ["Service", { resource: "services", namespaced: true }],
  ["PersistentVolumeClaim", { resource: "persistentvolumeclaims", namespaced: true }],
  ["Deployment", { resource: "deployments.apps", namespaced: true }],
  ["Role", { resource: "roles.rbac.authorization.k8s.io", namespaced: true }],
  ["RoleBinding", { resource: "rolebindings.rbac.authorization.k8s.io", namespaced: true }],
  ["NetworkPolicy", { resource: "networkpolicies.networking.k8s.io", namespaced: true }],
  ["Namespace", { resource: "namespaces", namespaced: false }],
  ["CustomResourceDefinition", { resource: "customresourcedefinitions.apiextensions.k8s.io", namespaced: false }],
  ["ClusterRole", { resource: "clusterroles.rbac.authorization.k8s.io", namespaced: false }],
  ["ClusterRoleBinding", { resource: "clusterrolebindings.rbac.authorization.k8s.io", namespaced: false }],
  ["ValidatingWebhookConfiguration", { resource: "validatingwebhookconfigurations.admissionregistration.k8s.io", namespaced: false }],
  ["ValidatingAdmissionPolicy", { resource: "validatingadmissionpolicies.admissionregistration.k8s.io", namespaced: false }],
  ["ValidatingAdmissionPolicyBinding", { resource: "validatingadmissionpolicybindings.admissionregistration.k8s.io", namespaced: false }],
]);

function purgePlans() {
  const now = Date.now();
  for (const [id, plan] of plans) if (now >= new Date(plan.expiresAt).getTime()) plans.delete(id);
  while (plans.size >= MAX_PLANS) plans.delete(plans.keys().next().value as string);
}

export function parseRenderedResources(yaml: string): RenderedResource[] {
  const resources: RenderedResource[] = [];
  for (const document of parseAllDocuments(yaml)) {
    const item = document.toJSON() as ChartResource | null;
    if (!item) continue;
    if (!item.kind || !item.metadata?.name) throw new Error("The rendered chart contains an object without a kind or name.");
    const identity = resourceNames.get(item.kind);
    if (!identity) throw new Error(`The rendered chart contains unsupported kind ${item.kind}.`);
    resources.push({
      kind: item.kind,
      resource: identity.resource,
      name: item.metadata.name,
      namespace: identity.namespaced ? item.metadata.namespace ?? "orka-system" : undefined,
      roleRef: bindingRoleRef(item, identity.namespaced ? item.metadata.namespace ?? "orka-system" : undefined),
    });
  }
  if (!resources.some((item) => item.kind === "Namespace" && item.name === "orka-system")) {
    resources.push({ kind: "Namespace", resource: "namespaces", name: "orka-system" });
  }
  return resources;
}

function bindingRoleRef(item: ChartResource, bindingNamespace?: string): RenderedResource["roleRef"] {
  if (item.kind !== "RoleBinding" && item.kind !== "ClusterRoleBinding") return undefined;
  const roleRef = item.roleRef;
  if (roleRef?.apiGroup !== "rbac.authorization.k8s.io" || !roleRef.name) {
    throw new Error(`${item.kind} ${item.metadata?.name ?? "unknown"} has an unsupported roleRef.`);
  }
  if (roleRef.kind === "Role" && bindingNamespace) {
    return { resource: "roles.rbac.authorization.k8s.io", name: roleRef.name, namespace: bindingNamespace };
  }
  if (roleRef.kind === "ClusterRole") {
    return {
      resource: "clusterroles.rbac.authorization.k8s.io",
      name: roleRef.name,
      namespace: item.kind === "RoleBinding" ? bindingNamespace : undefined,
    };
  }
  throw new Error(`${item.kind} ${item.metadata?.name ?? "unknown"} has an unsupported roleRef kind.`);
}

async function renderedResources(chartPath: string): Promise<RenderedResource[]> {
  const rendered = await runCommand(
    "helm",
    ["template", "orka", chartPath, "--namespace", "orka-system", "--include-crds", ...helmImageArgs()],
    { timeoutMs: 60_000, maxOutputBytes: 12_000_000 },
  );
  return parseRenderedResources(rendered.stdout);
}

async function collisionBlockers(context: string, chartPath: string): Promise<string[]> {
  const blockers: string[] = [];
  const resourcesByType = new Map<string, RenderedResource[]>();
  for (const item of (await renderedResources(chartPath)).filter((resource) => !resource.namespace)) {
    const items = resourcesByType.get(item.resource) ?? [];
    items.push(item);
    resourcesByType.set(item.resource, items);
  }
  for (const [resource, items] of resourcesByType) {
    try {
      const result = await runCommand("kubectl", [
        "--context", context, "--request-timeout=20s", "get", resource,
        ...items.map((item) => item.name), "--ignore-not-found", "-o", "name",
      ]);
      const existing = new Set(result.stdout.trim().split("\n").filter(Boolean).map((name) => name.split("/").at(-1)));
      for (const item of items.filter((candidate) => existing.has(candidate.name))) {
        blockers.push(`${item.kind} ${item.name} already exists.`);
      }
    } catch {
      blockers.push(`Could not prove rendered ${resource} objects are absent.`);
    }
  }
  try {
    const result = await runCommand("helm", ["list", "--kube-context", context, "--all-namespaces", "--filter", "^orka$", "--output", "json"]);
    if ((JSON.parse(result.stdout) as unknown[]).length > 0) blockers.push("An Orka Helm release already exists.");
  } catch {
    blockers.push("Could not inspect Helm releases on this cluster.");
  }
  return blockers;
}

export interface PermissionRequirement {
  verb: string;
  resource: string;
  resourceName?: string;
  namespace?: string;
}

export function permissionRequirements(resources: RenderedResource[]): PermissionRequirement[] {
  const checks = new Map<string, PermissionRequirement>();
  for (const item of resources) {
    for (const verb of ["get", "list", "watch", "create", "update", "patch", "delete"]) {
      const resourceName = ["get", "update", "patch", "delete"].includes(verb) ? item.name : undefined;
      const key = `${verb}\n${item.resource}\n${resourceName ?? "*"}\n${item.namespace ?? "cluster"}`;
      checks.set(key, { verb, resource: item.resource, resourceName, namespace: item.namespace });
    }
    if (item.kind === "Role" || item.kind === "ClusterRole") {
      checks.set(`escalate\n${item.resource}\n${item.name}\n${item.namespace ?? "cluster"}`, {
        verb: "escalate", resource: item.resource, resourceName: item.name, namespace: item.namespace,
      });
    }
    if (item.roleRef) {
      checks.set(`bind\n${item.roleRef.resource}\n${item.roleRef.name}\n${item.roleRef.namespace ?? "cluster"}`, {
        verb: "bind",
        resource: item.roleRef.resource,
        resourceName: item.roleRef.name,
        namespace: item.roleRef.namespace,
      });
    }
  }
  for (const verb of ["get", "list", "create", "update", "patch", "delete"]) {
    checks.set(`${verb}\nsecrets\norka-system`, { verb, resource: "secrets", namespace: "orka-system" });
  }
  return [...checks.values()];
}

async function renderedPermissionBlockers(context: string, chartPath: string): Promise<string[]> {
  let systemRules;
  let runtimeRules;
  try {
    [systemRules, runtimeRules] = await Promise.all([
      subjectRules(context, "orka-system"),
      subjectRules(context, "orka-runtimes"),
    ]);
  } catch {
    return ["Could not obtain complete Kubernetes authorization rules for both Orka namespaces."];
  }
  const blockers: string[] = [];
  const clusterChecks: PermissionRequirement[] = [];
  for (const check of permissionRequirements(await renderedResources(chartPath))) {
    if (!check.namespace) {
      clusterChecks.push(check);
      continue;
    }
    const rules = check.namespace === "orka-runtimes" ? runtimeRules : systemRules;
    if (!rulesAllow(rules, check.verb, check.resource, check.resourceName)) {
      blockers.push(`Missing permission: ${check.verb} ${check.resource}/${check.resourceName ?? "*"}${check.namespace ? ` in ${check.namespace}` : ""}.`);
    }
  }
  const clusterResults = await concurrentMap(clusterChecks, 8, async (check) => ({
    check,
    allowed: await accessAllowed(context, check),
  }));
  for (const { check, allowed } of clusterResults) {
    if (!allowed) blockers.push(`Missing permission: ${check.verb} ${check.resource}/${check.resourceName ?? "*"}.`);
  }
  return blockers;
}

async function concurrentMap<T, R>(items: T[], concurrency: number, operation: (item: T) => Promise<R>): Promise<R[]> {
  const results = new Array<R>(items.length);
  let next = 0;
  const workers = Array.from({ length: Math.min(concurrency, items.length) }, async () => {
    while (next < items.length) {
      const index = next++;
      results[index] = await operation(items[index]);
    }
  });
  await Promise.all(workers);
  return results;
}

export function rollbackFlagForVersion(version: string): "--atomic" | "--rollback-on-failure" {
  const major = Number(/^v?(\d+)/.exec(version.trim())?.[1]);
  if (major === 3) return "--atomic";
  if (major === 4) return "--rollback-on-failure";
  throw new Error("Quick Deploy requires a supported Helm 3 or Helm 4 release.");
}

async function helmRollbackFlag(): Promise<"--atomic" | "--rollback-on-failure"> {
  const result = await runCommand("helm", ["version", "--template", "{{.Version}}"], { timeoutMs: 10_000, maxOutputBytes: 10_000 });
  return rollbackFlagForVersion(result.stdout);
}

export async function createInstallPlan(contextName: string, ignoreInstallLock = false): Promise<InstallPlan> {
  purgePlans();
  const context = await requireContext(contextName);
  let chartPath: string | undefined;
  let rollbackFlag: "--atomic" | "--rollback-on-failure" | undefined;
  try {
    chartPath = await downloadVerifiedChart();
    await validateRenderedChart(chartPath);
    rollbackFlag = await helmRollbackFlag();
  } catch {
    // The blocker below keeps planning fail-closed while still returning the other findings.
  }
  const [report, fingerprint, collisions, permissionIssues] = await Promise.all([
    crawlCluster(contextName),
    clusterFingerprint(contextName),
    chartPath ? collisionBlockers(contextName, chartPath) : Promise.resolve(["The exact chart inventory could not be inspected."]),
    chartPath ? renderedPermissionBlockers(contextName, chartPath) : Promise.resolve(["The exact chart permissions could not be inspected."]),
  ]);
  const blockers = [...collisions, ...permissionIssues];
  if (report.orka.state !== "absent") blockers.push(
    report.orka.state === "partial"
      ? "Orka CRDs already exist without a live controller; an operator must reconcile them manually."
      : "An Orka installation already exists on this cluster.",
  );
  if (!report.defaultStorageClass) blockers.push("A default StorageClass is required for Orka data volumes.");
  const admissionResources = await runCommand("kubectl", ["--context", contextName, "api-resources", "-o", "name"])
    .then((result) => result.stdout)
    .catch(() => "");
  if (!admissionResources.includes("validatingadmissionpolicies.admissionregistration.k8s.io") || !admissionResources.includes("validatingadmissionpolicybindings.admissionregistration.k8s.io")) {
    blockers.push(`This cluster does not expose the admission policy APIs required by Orka ${release.version}.`);
  }
  if (!rollbackFlag) blockers.push("The pinned chart, images, or supported Helm version could not be verified.");
  if (installInProgress && !ignoreInstallLock) blockers.push("Another Orka installation is already in progress.");

  const plan: InstallPlan = {
    id: randomBytes(18).toString("base64url"),
    context: context.name,
    cluster: context.cluster,
    server: context.server,
    fingerprint,
    namespace: "orka-system",
    runtimeNamespace: "orka-runtimes",
    releaseName: "orka",
    chartVersion: release.version,
    chartDigest: release.chartDigest,
    expiresAt: new Date(Date.now() + PLAN_TTL_MS).toISOString(),
    allowed: blockers.length === 0,
    blockers,
    warnings: [
      "The chart creates CRDs, namespaces, cluster-wide RBAC, admission policies, webhooks, and persistent volumes.",
      "Helm does not roll back CRDs automatically; a failed installation can require manual CRD reconciliation.",
      "Orka is experimental. This console does not offer automatic upgrade, repair, or uninstall.",
    ],
  };
  plans.set(plan.id, plan);
  return plan;
}

async function downloadVerifiedChart(): Promise<string> {
  if (!processChartDirectory) {
    processChartDirectory = mkdtemp(join(tmpdir(), "orka-control-")).then(async (directory) => {
      await chmod(directory, 0o700);
      return directory;
    });
  }
  const directory = await processChartDirectory;
  const chartPath = join(directory, `orka-${release.version}.tgz`);
  try {
    const existing = await readFile(chartPath);
    if (createHash("sha256").update(existing).digest("hex") === release.chartDigest) return chartPath;
  } catch {
    // Download the pinned chart below.
  }
  const response = await fetch(release.chartUrl, { redirect: "error", signal: AbortSignal.timeout(30_000) });
  if (!response.ok) throw new Error(`Could not download the pinned Orka chart (${response.status}).`);
  const length = Number(response.headers.get("content-length") ?? 0);
  if (length > 10_000_000) throw new Error("The pinned Orka chart exceeds the download limit.");
  const bytes = Buffer.from(await response.arrayBuffer());
  if (bytes.length > 10_000_000) throw new Error("The pinned Orka chart exceeds the download limit.");
  if (createHash("sha256").update(bytes).digest("hex") !== release.chartDigest) {
    throw new Error("The downloaded Orka chart failed digest verification.");
  }
  const chartFile = await open(chartPath, "wx", 0o600);
  try { await chartFile.writeFile(bytes); }
  finally { await chartFile.close(); }
  return chartPath;
}

async function assertVerifiedChart(chartPath: string): Promise<void> {
  const bytes = await readFile(chartPath);
  if (createHash("sha256").update(bytes).digest("hex") !== release.chartDigest) {
    throw new Error("The local Orka chart changed after verification.");
  }
}

async function validateRenderedChart(chartPath: string): Promise<void> {
  const rendered = await runCommand(
    "helm",
    ["template", "orka", chartPath, "--namespace", "orka-system", "--include-crds", ...helmImageArgs()],
    { timeoutMs: 60_000, maxOutputBytes: 12_000_000 },
  );
  const images: string[] = [];
  const visit = (value: unknown) => {
    if (Array.isArray(value)) return value.forEach(visit);
    if (!value || typeof value !== "object") return;
    for (const nested of Object.values(value as Record<string, unknown>)) {
      if (typeof nested === "string" && nested.includes("ghcr.io/orka-agents/orka")) images.push(nested);
      visit(nested);
    }
  };
  for (const document of parseAllDocuments(rendered.stdout)) visit(document.toJSON());
  if (images.some((image) => !image.includes("@sha256:"))) {
    throw new Error("The rendered Orka chart contains unpinned release images.");
  }
}

export async function installOrka(input: {
  planId: string;
  context: string;
  confirmation: string;
  acknowledgeClusterAccess: boolean;
  acknowledgePersistence: boolean;
}): Promise<ClusterReport> {
  const plan = plans.get(input.planId);
  plans.delete(input.planId);
  if (!plan) throw new Error("The installation plan is missing or was already used. Create a new plan.");
  if (Date.now() >= new Date(plan.expiresAt).getTime()) throw new Error("The installation plan expired. Create a new plan.");
  if (!plan.allowed) throw new Error("This installation plan contains blockers and cannot be executed.");
  if (input.context !== plan.context || input.confirmation !== plan.context) throw new Error("The typed Kubernetes context does not match the installation plan.");
  if (!input.acknowledgeClusterAccess || !input.acknowledgePersistence) throw new Error("Both installation acknowledgements are required.");
  if (installInProgress) throw new Error("Another Orka installation is already in progress.");

  installInProgress = true;
  try {
    const [freshPlan, fingerprint] = await Promise.all([
      createInstallPlan(plan.context, true),
      clusterFingerprint(plan.context),
    ]);
    plans.delete(freshPlan.id);
    if (!freshPlan.allowed || fingerprint !== plan.fingerprint) throw new Error("Cluster state changed after planning. Review a new installation plan.");
    const chartPath = await downloadVerifiedChart();
    await assertVerifiedChart(chartPath);
    await validateRenderedChart(chartPath);
    const rollbackFlag = await helmRollbackFlag();
    await runCommand("helm", [
      "install", "orka", chartPath,
      "--kube-context", plan.context,
      "--namespace", "orka-system",
      "--create-namespace",
      "--wait",
      rollbackFlag,
      "--timeout", "10m",
      ...helmImageArgs(),
    ], { timeoutMs: 21 * 60_000, terminationGraceMs: 10 * 60_000, maxOutputBytes: 1_000_000 });
    return await crawlCluster(plan.context, "orka-system");
  } finally {
    installInProgress = false;
  }
}

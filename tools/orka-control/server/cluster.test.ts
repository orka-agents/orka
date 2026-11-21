import assert from "node:assert/strict";
import test from "node:test";
import { canonicalIdempotencyKey, normalizeTask, rulesAllow, sanitizeServer, selectControllerDeployment, stableUserInfo, taskCountsAgainstQuota, taskNameForRequest, type AuthorizationRule } from "./cluster.js";

const wildcardRule: AuthorizationRule = {
  verbs: ["*"],
  apiGroups: ["*"],
  resources: ["*"],
};

test("rulesAllow accepts wildcard authorization", () => {
  assert.equal(rulesAllow([wildcardRule], "create", "tasks.core.orka.ai"), true);
});

test("rulesAllow enforces API group and resource name", () => {
  const rules: AuthorizationRule[] = [{
    verbs: ["get", "patch"],
    apiGroups: ["rbac.authorization.k8s.io"],
    resources: ["clusterroles"],
    resourceNames: ["orka-controller"],
  }];
  assert.equal(rulesAllow(rules, "get", "clusterroles.rbac.authorization.k8s.io", "orka-controller"), true);
  assert.equal(rulesAllow(rules, "get", "clusterroles.rbac.authorization.k8s.io", "other"), false);
  assert.equal(rulesAllow(rules, "get", "roles.rbac.authorization.k8s.io", "orka-controller"), false);
  assert.equal(rulesAllow(rules, "delete", "clusterroles.rbac.authorization.k8s.io", "orka-controller"), false);
});

test("unknown and active Tasks consume quota", () => {
  for (const phase of [undefined, "Unknown", "Pending", "Running", "Finalizing"]) {
    const task = normalizeTask({
      metadata: { name: `task-${phase ?? "none"}` },
      spec: { type: "container" },
      status: phase ? { phase } : undefined,
    });
    assert.equal(taskCountsAgainstQuota(task), true, `${phase ?? "missing"} should consume quota`);
  }
});

test("terminal and scheduled Tasks release quota", () => {
  for (const phase of ["Succeeded", "Failed", "Cancelled", "Scheduled"]) {
    const task = normalizeTask({ metadata: { name: `task-${phase}` }, status: { phase } });
    assert.equal(taskCountsAgainstQuota(task), false, `${phase} should release quota`);
  }
});

test("stableUserInfo ignores response metadata and sorts groups and extras", () => {
  const first = stableUserInfo({
    status: { userInfo: { username: "operator", uid: "42", groups: ["z", "a"], extra: { b: ["2", "1"], a: ["x"] } } },
    metadata: { creationTimestamp: "first" },
  } as never);
  const second = stableUserInfo({
    status: { userInfo: { username: "operator", uid: "42", groups: ["a", "z"], extra: { a: ["x"], b: ["1", "2"] } } },
    metadata: { creationTimestamp: "second" },
  } as never);
  assert.deepEqual(first, second);
});

test("controller discovery supports Helm and Kustomize labels without arbitrary fallback", () => {
  const wrapper = { metadata: { name: "wrapper", labels: { "app.kubernetes.io/name": "orka" } } };
  const kustomizeController = { metadata: { name: "manager", labels: { "app.kubernetes.io/name": "orka", "control-plane": "controller-manager" } } };
  const helmController = { metadata: { name: "controller", labels: { "app.kubernetes.io/name": "orka", "app.kubernetes.io/component": "controller" } } };
  assert.equal(selectControllerDeployment([wrapper, kustomizeController])?.metadata.name, "manager");
  assert.equal(selectControllerDeployment([wrapper, kustomizeController, helmController])?.metadata.name, "controller");
  assert.equal(selectControllerDeployment([wrapper]), undefined);
});

test("Task names are deterministic and DNS safe", () => {
  assert.equal(canonicalIdempotencyKey("550E8400-E29B-41D4-A716-446655440000"), "550e8400-e29b-41d4-a716-446655440000");
  assert.equal(taskNameForRequest("550E8400-E29B-41D4-A716-446655440000"), "console-550e8400-e29b-41d4-a716-446655440000");
});

test("cluster server projection removes URL credentials", () => {
  assert.equal(sanitizeServer("https://user:secret@example.test:6443/"), "https://example.test:6443");
  assert.equal(sanitizeServer("not a URL"), "unknown");
});

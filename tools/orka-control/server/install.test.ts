import assert from "node:assert/strict";
import test from "node:test";
import { parseRenderedResources, permissionRequirements, rollbackFlagForVersion } from "./install.js";
import { release } from "./release.js";

test("release manifest pins chart, images, and stable CRDs", () => {
  assert.match(release.chartDigest, /^[a-f0-9]{64}$/);
  assert.equal(release.crds.length, 27);
  for (const value of release.imageValues.filter((value) => value.includes("Image=") || value.includes(".digest="))) {
    assert.match(value, /sha256:[a-f0-9]{64}$/);
  }
});

test("rendered inventory includes CRDs and synthesizes the release namespace", () => {
  const resources = parseRenderedResources(`
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: tasks.core.orka.ai
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: orka-controller
  namespace: orka-system
`);
  assert.deepEqual(resources.map((resource) => [resource.kind, resource.name]), [
    ["CustomResourceDefinition", "tasks.core.orka.ai"],
    ["Deployment", "orka-controller"],
    ["Namespace", "orka-system"],
  ]);
});

test("rendered inventory rejects unsupported kinds", () => {
  assert.throws(() => parseRenderedResources(`
apiVersion: batch/v1
kind: Job
metadata:
  name: unexpected-hook
`), /unsupported kind Job/);
});

test("permission inventory covers readiness, mutation, and rollback verbs", () => {
  const requirements = permissionRequirements([{
    kind: "Deployment",
    resource: "deployments.apps",
    name: "orka-controller",
    namespace: "orka-system",
  }]);
  for (const verb of ["get", "list", "watch", "create", "update", "patch", "delete"]) {
    assert.ok(requirements.some((requirement) => requirement.verb === verb && requirement.resource === "deployments.apps"));
  }
  assert.ok(requirements.some((requirement) => requirement.resource === "secrets" && requirement.verb === "create"));
});

test("binding permissions target the exact referenced role", () => {
  const resources = parseRenderedResources(`
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: orka-runtime-reader
  namespace: orka-runtimes
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: orka-runtime-reader
subjects: []
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: orka-controller
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: orka-controller
subjects: []
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: orka-runtime-cluster-reader
  namespace: orka-runtimes
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: orka-runtime-cluster-reader
subjects: []
`);
  const requirements = permissionRequirements(resources);
  assert.ok(requirements.some((requirement) => requirement.verb === "bind" && requirement.resource === "roles.rbac.authorization.k8s.io" && requirement.resourceName === "orka-runtime-reader" && requirement.namespace === "orka-runtimes"));
  assert.ok(requirements.some((requirement) => requirement.verb === "bind" && requirement.resource === "clusterroles.rbac.authorization.k8s.io" && requirement.resourceName === "orka-controller" && requirement.namespace === undefined));
  assert.ok(requirements.some((requirement) => requirement.verb === "bind" && requirement.resource === "clusterroles.rbac.authorization.k8s.io" && requirement.resourceName === "orka-runtime-cluster-reader" && requirement.namespace === "orka-runtimes"));
});

test("Helm major versions select supported rollback semantics", () => {
  assert.equal(rollbackFlagForVersion("v3.19.0"), "--atomic");
  assert.equal(rollbackFlagForVersion("v4.1.0"), "--rollback-on-failure");
  assert.throws(() => rollbackFlagForVersion("v5.0.0"), /supported Helm 3 or Helm 4/);
});

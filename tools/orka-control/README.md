# Orka Control

Orka Control is an experimental, local-only companion for discovering, bootstrapping, and smoke-testing Orka on Kubernetes. It addresses [#1236](https://github.com/orka-agents/orka/issues/1236).

It does not replace the embedded Orka dashboard. Use it before installation or when the controller is unavailable; use the embedded dashboard for normal Orka operation.

## Modes

- **Crawl** reads the selected kubeconfig context and reports Orka CRDs, controller readiness, Tasks, Agents, components, storage, and permissions.
- **Run** creates either a fixed model-free smoke Task or a bounded prompt Task for an existing Agent.
- **Agent pod** maps real Agents and active Tasks into an optional aquarium-style status view.
- **Quick deploy** creates a five-minute installation plan and installs the reviewed release only after typed context confirmation and explicit risk acknowledgements.

## Requirements

- Node.js 22+
- Bun 1.3+
- `kubectl` with a usable kubeconfig context
- Helm 3 or Helm 4 for Quick Deploy

## Run locally

```bash
cd tools/orka-control
bun install --frozen-lockfile
bun run dev
```

The gateway selects a random `127.0.0.1` port and opens an authenticated browser session. Vite runs only as a build watcher, so the UI and API share one unpredictable loopback origin.

For a local production build:

```bash
bun run check
bun run start
```

## Security boundary

This gateway uses the operator's local kubeconfig and is intentionally unsuitable for public or multi-user hosting.

- Every API request requires a random per-launch capability.
- Kubeconfig credentials, bearer tokens, Secret data, and chart values never enter the browser.
- Kubernetes and Helm commands use fixed argument arrays with `shell: false`.
- Request schemas, body sizes, rate, concurrency, task quotas, and output sizes are bounded.
- The five-active-Task limit is a single-process UX guardrail, not a cluster-wide policy. Enforce cost or tenant limits with Kubernetes admission or controller policy.
- Quick Deploy fails closed on incomplete discovery, resource collisions, missing authorization, unsupported rendered kinds, unsupported Helm versions, or digest mismatches.
- The release manifest pins the chart and all release-owned images by SHA-256 digest.
- Install plans are expiring and single-use, and a fresh preflight runs immediately before Helm.
- Upgrades, repairs, and uninstall are deliberately unsupported.

Do not bind, proxy, tunnel, or publish the local gateway. A hosted console needs user authentication, short-lived per-user cluster identities, tenant and namespace authorization, CSRF controls, quotas, audit logs, and a separate ephemeral installer identity.

## Install impact

Quick Deploy creates Orka CRDs, the `orka-system` and `orka-runtimes` namespaces, cluster-wide RBAC, admission policy and webhooks, Secrets, and persistent volumes. Helm does not roll back CRDs automatically; a failed installation can require manual reconciliation.

The reviewed release contract lives in `server/release.ts`. Updating it requires verifying the official release candidate metadata, chart digest, complete CRD inventory, and every release image digest together.

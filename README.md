<div align="center">

<img src="website/static/img/orka-logo.png" alt="Orka" width="400" />

# Orka

**Run AI agents and coding agents on your cluster. Model keys never leave it.**

[![Tests](https://github.com/orka-agents/orka/actions/workflows/test.yml/badge.svg)](https://github.com/orka-agents/orka/actions/workflows/test.yml)
[![Release](https://img.shields.io/github/v/release/orka-agents/orka?include_prereleases)](https://github.com/orka-agents/orka/releases)
[![License](https://img.shields.io/github/license/orka-agents/orka)](LICENSE)

[Getting started](https://orka-agents.github.io/orka/docs/getting-started) · [Install](https://orka-agents.github.io/orka/docs/installation) · [Docs](https://orka-agents.github.io/orka/) · [Contributing](CONTRIBUTING.md)

</div>

---

Orka turns a Kubernetes cluster into a place to run AI agents. You describe work as a
**Task**; Orka runs it in a Pod, keeps a durable record of what happened, and hands you
the result over a REST API, a CLI, or the built-in dashboard. Tasks can be Orka's own AI
worker, a real coding-agent CLI such as Codex, Claude Code, GitHub Copilot CLI, or
OpenCode, or any container command.

Model credentials stay in the cluster. Developers get a ServiceAccount token, not an API
key, and the platform team decides which models and providers are allowed.

> [!IMPORTANT]
> **Orka is experimental and under active development.** APIs, CRDs, and behavior may change without notice between releases, and it is not yet recommended for production use. Feedback, bug reports, and feature ideas are very welcome — please [open an issue](https://github.com/orka-agents/orka/issues).

> [!NOTE]
> The organization and repositories are intended to be donated to a community-governed foundation at the appropriate time. Until then, the project is governed by Microsoft policy, and external contributors are required to sign the Microsoft Contributor License Agreement (CLA).

## Quick start

```bash
helm repo add orka https://orka-agents.github.io/orka/charts
helm repo update orka
helm install orka orka/orka --namespace orka-system --create-namespace --wait --timeout 10m
```

That is the whole install. The chart creates its namespaces, its encryption key, and its
webhook certificate. Run something, no model key needed:

```bash
kubectl -n orka-system apply -f - <<'EOF'
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: hello
spec:
  type: container
  command: ["sh", "-c", "echo hello from orka"]
EOF
kubectl -n orka-system get task hello --watch
```

Then follow [Getting started](https://orka-agents.github.io/orka/docs/getting-started)
to connect to the API, add a model provider, and run your first AI Task.

## Why Kubernetes

- **No keys on laptops.** Provider credentials live in Secrets; people and CI get scoped tokens.
- **One place to govern.** Models, providers, tools, and limits are set per Agent and per namespace.
- **Everything is a record.** Tasks, sessions, artifacts, and the pull requests agents open are durable and auditable.
- **Scale with the cluster.** Scheduling, retries, concurrency, and cron come from the control plane you already run.

## Learn more

- [Architecture](https://orka-agents.github.io/orka/docs/architecture) — how a Task becomes a Pod
- [Interactive chat](https://orka-agents.github.io/orka/docs/chat) — describe what you want and let an orchestrator create the Tasks
- [Coding agents](https://orka-agents.github.io/orka/docs/agent-runtimes) — Codex, Claude Code, Copilot, and OpenCode as pooled runtimes
- [Compatibility APIs](https://orka-agents.github.io/orka/docs/openai-compat) — point Cursor, Continue, or Claude Code at Orka
- [Security](https://orka-agents.github.io/orka/docs/security) — the trust model and hardening
- [Troubleshooting](https://orka-agents.github.io/orka/docs/troubleshooting) — error strings, causes, fixes
- [Development](https://orka-agents.github.io/orka/docs/development) — building, testing, and contributing
- [Security policy](SECURITY.md) — how to report a vulnerability

## License

[MIT](LICENSE)

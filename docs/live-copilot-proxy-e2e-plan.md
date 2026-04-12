# Live Copilot Proxy E2E Plan

## Goal

Add a separate GitHub Actions workflow that validates Orka against a live
multi-model backend in Kubernetes with a real GitHub token.

`copilot-proxy` is harness infrastructure in this workflow, not the main product
under test. We use it so Orka's non-Copilot harnesses can talk to live GPT,
Claude, and Gemini models in CI through one backend.

Primary path under test:

`test -> Orka -> Provider -> copilot-proxy -> GitHub Copilot backend -> model response -> Orka result`

## Scope

This plan covers live Orka paths enabled by the proxy-backed backend.

In scope:

- Create a fresh Kind cluster in GitHub Actions
- Deploy Orka into the cluster
- Deploy `copilot-proxy` into the cluster
- Inject a real GitHub token into `copilot-proxy`
- Wait for proxy readiness and confirm live models are available
- Create an Orka `Provider` CRD that points to the proxy service
- Run only the focused live Orka E2E spec(s)
- Collect failure diagnostics with secret redaction

Out of scope:

- Broad proxy feature coverage
- Exhaustive per-model behavior validation
- Testing the proxy as a standalone product surface

## Current Implemented Scenarios

The PR-blocking live workflow currently runs these Orka scenarios:

1. Provider-backed `type: ai` success path
   - proxy readiness and live model discovery
   - Orka `Provider` readiness
   - one tiny Orka `type: ai` task with exact output assertion

2. Chat API live path
   - `POST /api/v1/chat` in SSE mode
   - `POST /api/v1/chat` in JSON mode
   - session creation and persistence
   - usage/transport assertions against a live backend

3. Agent runtime matrix
   - `codex` runtime with a GPT-family model and `priorTaskRef`
   - `claude` runtime with a Claude-family model and `sessionRef`
   - `copilot` runtime with a Gemini-family model on a pinned repo checkout

The only proxy-specific checks are harness smoke checks:

- `/readyz`
- `/v1/models`
- GPT, Claude, and Gemini model families are present

## Workflow Shape

Create a separate GitHub Actions workflow dedicated to this live validation.

High-level workflow steps:

1. Check out the repository
2. Set up Go and Kind
3. Create a fresh Kind cluster
4. Deploy Orka using the existing E2E bootstrap flow
5. Deploy `copilot-proxy` into the same cluster
6. Inject the GitHub token from workflow secrets into the proxy
7. Wait for `/readyz`
8. Confirm `/v1/models` returns GPT, Claude, and Gemini model families
9. Run only the focused live E2E test(s)
10. Print redacted diagnostics on failure
11. Tear the cluster down

## Provider Setup

The live test should configure an Orka `Provider` CRD that treats
`copilot-proxy` as an OpenAI-compatible backend:

- `type: openai`
- `baseURL`: the in-cluster `copilot-proxy` service endpoint
- secret-backed dummy API key if required by Orka's provider validation

The test should not hard-code a model when that would make the workflow fragile.
Instead, it should discover an available model from `copilot-proxy` and use that.

## What This Validates

This workflow should prove:

- `copilot-proxy` can authenticate in cluster with the supplied GitHub token
- `copilot-proxy` can surface live models from GitHub Copilot
- Orka can route requests to the proxy through a `Provider`
- Orka chat can execute against a live backend
- Orka agent runtimes can execute against GPT, Claude, and Gemini families
- a real model response comes back through the full stack

## GitHub Actions Requirements

Expected secret:

- `COPILOT_GITHUB_TOKEN` or similarly named repository secret for proxy auth

Recommended workflow behavior:

- keep this workflow separate from the default E2E workflow
- run only the focused live Orka test(s), not the entire suite
- add failure logs for Orka, proxy, pods, and events
- redact tokens and auth headers before printing logs

## Current Decision Summary

We intentionally chose a separate workflow, even though it duplicates Kind setup,
to keep the live GitHub-token-backed validation simple and isolated from the
existing deterministic E2E pipeline.

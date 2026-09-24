---
slug: /demos
title: Demos
description: "Watch Orka demos by feature, with links to the setup and reference documentation."
---

# Demos

Watch Orka workflows on Kubernetes, grouped by feature. Each demo links to the
documentation for its setup and behavior.

[Watch the full playlist on YouTube](https://www.youtube.com/playlist?list=PLR43VQQvpyY8).

## Chat and pull requests

[From chat to a GitHub pull request](https://www.youtube.com/watch?v=FlLgdh2lKMk)

Follow a coding-client request through coding and review Tasks, then verify the
Publisher's pull request on GitHub. The client uses a cluster token rather than
a model API key.

Read about [interactive chat](guides/chat.md), the
[Anthropic-compatible API](reference/anthropic-compat.md), and
[workspace publication](concepts/agent-runtimes.md#write-and-publication-task).

## Execution workspaces

[Suspend an agent workspace and keep its files](https://www.youtube.com/watch?v=DyS9JioSRa0)

Stop an idle worker, resume the same Session with its files intact, and clean up
the workspace when the work is done. This demo uses
[Agent Sandbox workspaces](concepts/agent-sandbox.md).

[Checkpoint and restore an agent workspace](https://www.youtube.com/watch?v=jsdRB-0LLAc)

Save an independent data-only checkpoint, delete the original workspace, and
restore its files into a new workspace. This demo uses
[Agent Substrate workspaces](concepts/substrate.md). It preserves files, not a
running process or its memory.

## Repository security scanning

[Turn a security finding into a pull request](https://www.youtube.com/watch?v=o7C_Kot_h8M)

Register a test repository, review its threat model and validated findings, then
choose a finding to fix and open a remediation pull request.

Read the [repository security scanning guide](guides/repository-security-scanning.md).

## Shared AI endpoints

[Two teams, one AI endpoint](https://www.youtube.com/watch?v=-x0tKk9epWU)

Route each caller to its team's Orka installation through one endpoint, then
check that requests for the other team's models and namespace are refused.

Read about [shared OpenAI and Anthropic endpoints](operations/shared-compatibility-api.md).

## Agent-to-agent requests

[Agent-to-agent requests with safe retries](https://www.youtube.com/watch?v=2shxIt1ooxE)

Discover an Agent Card, ask an inventory agent for advice through A2A, retry
without duplicate work, and continue the conversation in the same Session.

Read about the [A2A gateway adapter](operations/gateways.md#external-adapters).

## Tool access controls

[Allow stock checks but block purchasing](https://www.youtube.com/watch?v=1vDI6PxhmfY)

Expose a stock lookup while keeping the supplier's purchasing route closed.
Verify that the lookup reaches the test supplier and no order is created.

Read about [Tool configuration](reference/configuration.md#tool) and
[outbound access policies](concepts/outbound-access.md).

## Memory

[Review and publish an agent's memory](https://www.youtube.com/watch?v=iCqyVfWBpyg)

Let an agent propose a note, review it, and explicitly apply it before a fresh
agent can use the shared knowledge. Accepting a proposal alone does not publish it.

Read about [memory proposals and durable memory](concepts/memory.md).

## Human approval

[Human approval before an agent takes action](https://www.youtube.com/watch?v=KgzcvW1WznE)

Hold a proposed work order for review, record a person's approval, and verify
that exactly one order is created. The result returns to the original waiting Task.

Read the [human approval guide](guides/human-approval-v2.md).

## Provider routing

[Compare hosted and routed agents on the same workload](https://www.youtube.com/watch?v=L6W3IPuLmeQ)

Compare hosted execution with Vekil routing between local AIKit inference and a
hosted model, including fallback behavior, elapsed time, and estimated cost.

Read the [provider proxy guide](operations/provider-proxy.md).

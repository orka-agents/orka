---
description: "Migrating transaction-token profiles, TTS endpoints, and restricted tool grants."
---

# Transaction-token migration

## Generic transaction-token profile

This release intentionally removes the in-tree Kontxt profile and deployment assets.

1. Change `--context-token-profile=kontxt` to `--context-token-profile=transaction-token` (or the matching environment/Helm value).
2. Replace the former TTS base URL with the exact endpoint:
   - `--context-token-tts-endpoint`
   - `ORKA_CONTEXT_TOKEN_TTS_ENDPOINT`
   - `controller.contextToken.tts.endpoint`
3. Move provider installation, signing/JWKS, and live E2E configuration to [`orka-agents/orka-integration-kontxt`](https://github.com/orka-agents/orka-integration-kontxt).
4. Use [`orka-agents/orka-integration-agentgateway`](https://github.com/orka-agents/orka-integration-agentgateway) for gateway routing and downstream OAuth exchange examples.
5. Remove the old URL flag/environment/value; there is no compatibility alias.

Rollback requires deploying the previous Orka release and restoring its old Kontxt configuration. Do not run the previous controller with the new exact-endpoint-only values.

## Restricted tool grants

Before upgrading, update issuer policies for restricted `tctx.allowedTools`
grants to cover the effective tool set. Task admission and delegated child
validation include Orka's implicit capabilities. Tokens that omit a required
capability are rejected.

In addition to explicitly selected tools and effective runtime tools, include
the following implicit capabilities for each task:

- Every `type: ai` task includes `recall_memory`, `remember`, `propose_memory`,
  and `search_transcript`.
- AI tasks whose Agent has `coordination.enabled: true` also include
  `delegate_task`, `wait_for_tasks`, `create_container_task`, `cancel_task`,
  `send_message`, `check_messages`, `create_pull_request`, `list_pull_requests`,
  `check_pr_review_marker`, `check_pull_request_ci`, `merge_pull_request`,
  `auto_merge_pull_request`, `review_pull_request`, `post_review_comment`,
  `create_agent`, `delete_agent`, and `update_plan`.
- Enabled AI coordination with `coordination.autonomous: true` also includes
  `request_approval`.
- Delegated AI tasks include `send_message` and `check_messages`, even without
  Agent coordination. Built-in ACP agent tasks with the `orka.ai/parent-task`
  label also receive these two messaging tools. They do not receive the AI
  worker's full coordination or memory tool set implicitly.

The annotation `orka.ai/disable-coordination-tool-injection: "true"` suppresses
implicit coordination, approval, and child messaging. AI memory tools remain
enabled, and explicitly selected tools still require grants. Agent tasks using
`runtimeRef` receive no implicit coordination or child messaging; their
registered runtime tool policy remains authoritative.

Reissue affected restricted tokens with the capabilities their tasks need, or
narrow the task configuration. Child grants must remain subsets of the parent
grant. Verify representative tasks are accepted with the intended grant and
denied when a required tool is omitted.

An empty `allowedTools: []` remains a valid, explicit deny-all constraint.
Preserve the empty array when issuing or forwarding tokens; omitting the claim
removes that tool restriction. A deny-all grant rejects any task with a nonempty
effective tool set, including AI tasks with their required memory tools.
Malformed authorization constraints fail token validation.

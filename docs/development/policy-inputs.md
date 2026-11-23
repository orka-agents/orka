# Policy inputs: approvals on the native AI path

This page describes the two inputs that decide a native AI Job's approval set,
which the [policy contract](policy-contract.md) (statement 7) relies on. Code
references are at commit `d31cf6b`; paths are relative to the repository root.

Both inputs are enforce-only: neither has an audit mode, and there is no
namespace- or installation-level approval setting.

## The Agent's approval list

| | |
| --- | --- |
| Input | `Agent.spec.coordination.approvalRequiredTools` (`api/v1alpha1/agent_types.go:523-527`) |
| Controls | Custom tools that need a human approval before they run |
| Level | Agent; written by anyone who may `update` Agents |
| Takes effect | Native AI: when the Job is built, as `ORKA_APPROVAL_REQUIRED_TOOLS` (`internal/controller/job_builder.go:1239-1241`). A recovered Job keeps the set it was built with (`internal/controller/task_job_identity.go:84-95`). |
| Enforced where | Native AI: the approval gate in the worker Pod (`workers/ai/approval_gate.go:73-90`) |
| Combines how | Native AI: merged with the offered connector write tools, sorted and deduplicated (`internal/controller/job_builder.go:1239-1241`) |
| Preconditions | Coordination must be enabled and autonomous, or the Task fails (`internal/controller/task_controller.go:4170-4172`). Built-in tool names are rejected (`:4173-4175`). |
| Other paths | `type: agent` Tasks fail with "approvalRequiredTools is only supported for type: ai autonomous tasks" (`internal/controller/task_controller.go:4108-4110`). |
| If unreadable | A Job that requires approvals but has no Task UID is a worker error (`workers/ai/approval_gate.go:82-84`) |
| Tests | `workers/ai/approval_gate_test.go`; the contract tests listed in [Policy contract](policy-contract.md#tests) |
| Related | #1267, #770, #698 |

## Connector write approval

| | |
| --- | --- |
| Input | A code rule over connector tools of class `write` (`ConnectorProvider.spec.tools[].class`, `api/v1alpha1/connector_provider_types.go:157-159`), plus the mode of the requester's `Connection` |
| Controls | Write-class connector tools are hidden for a read-only link and on any execution that cannot hold a call for approval. On native AI, the offered ones are added to the approval set. |
| Level | Code, not configurable. The person who owns the Connection sets its mode. |
| Takes effect | Native AI: when the Job is built, from the Connection bindings frozen for it (`internal/controller/job_builder.go:1294-1315`) |
| Enforced where | Native AI: visibility at `internal/controller/connection_freeze.go:876-878`; the approval set at `internal/controller/job_builder.go:1363-1367` |
| Combines how | Native AI: offered write tools are merged into `ORKA_APPROVAL_REQUIRED_TOOLS` with the Agent's list (`internal/controller/job_builder.go:1240`) |
| Preconditions | Native AI: only an Agent with enabled autonomous coordination is offered write tools, because only such a Task can wait for an approval and resume (`internal/controller/job_builder.go:1308-1315`) |
| On violation | Hidden, not refused |
| If unreadable | A tool read failure fails the Job build, and dispatch retries (`internal/controller/job_builder.go:1302-1306`) |
| Tests | `internal/controller/connector_tools_test.go`; `TestJobBuilder_buildEnvVars_ConnectorWriteToolsRequireApprovalAndHideOnReadOnly`; the contract tests |
| Related | #698, #770 |

## Where the sources differ

The `ConnectorProvider` API comment says write tools "default into the Agent's
approval-required set" (`api/v1alpha1/connector_provider_types.go:157-158`).
That describes the native AI path. Other paths treat them differently:

- Built-in coding-agent runtimes are not offered connector write tools
  (`internal/controller/acp_mcp_policy.go:268-275`).
- An external `runtimeRef` runtime requires each linked write tool to be
  listed in the registration's `approvalRequiredTools`, or the Task fails with
  a configuration error
  (`internal/controller/agent_execution_binding.go:1711-1722`).

Those paths are outside the tests on this page.

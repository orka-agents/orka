# aimock fixtures for the E2E suite

When `E2E_OPENAI_API_KEY` or `E2E_ANTHROPIC_API_KEY` is unset, the suite
deploys [aimock](https://github.com/CopilotKit/aimock) into the test namespace
and points that provider at it. Set `E2E_DISABLE_MOCK_LLM=1` to skip those
specs instead. aimock loads every `*.json` file here in name order, and the
first matching fixture wins, so `zz-catch-all.json` must stay last.

Conventions:

- One file per spec file, named after it (`tools.json` for `tools_test.go`).
- Every scripted prompt carries a unique `[e2e:<case>]` marker, and its
  fixtures match on it with `userMessage`. Do not reuse a marker across specs.
- Script multi-step tool loops with stateless matchers (`hasToolResult`,
  `turnIndex`, `toolCallId`, `toolResultContains`). The suite sets
  `AIMOCK_STRICT_TURN_INDEX=1`, so `turnIndex` matches only its exact turn. Do
  not use `sequenceIndex`: its server-side counter breaks on reruns and retries.
- Steps that must name a generated resource (a delegated child Task, a created
  Agent) are injected at run time with `injectMockLLMFixtures` once the name is
  known, while the conversation is parked in a scripted wait.
- Final turns return non-empty text. An empty answer makes the AI worker send a
  retry prompt that no longer carries the marker.
- In mock mode (`e2eMockOpenAI`), specs assert on what reached the model with
  `expectMockLLMToolResult` and `expectMockLLMServed` from `mock_llm_test.go`.

The fixture format is documented in aimock's
[write-fixtures skill](https://github.com/CopilotKit/aimock/blob/v1.43.0/skills/write-fixtures/SKILL.md).

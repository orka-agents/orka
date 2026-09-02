# ADR 0002: Report provider-neutral Execution Workspace status

## Status

Superseded for built-in agent Tasks by ACP v2 execution and delivery status.

The earlier prototype reported worker-owned upstream workspace lifecycle. Current ACP agent Tasks expose provider-neutral control state through:

- `Task.status.execution` for the fenced attempt, RuntimePool, RuntimeSession, prompt, and outcome;
- `Task.status.delivery` for workspace validation, clean-room publication, verification, and PR receipts;
- `RuntimePool.status` for lifecycle, admission, exact instance, capacity, and pressure.

Future execution-workspace providers must project into these Orka-owned surfaces and must not expose provider-native snapshot URIs, daemon URLs, credentials, or mutable child-controlled Git state.

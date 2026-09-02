# ADR 0003: Use the provider route for Workspace Daemon calls

## Status

Deferred with the Substrate ACP integration.

The earlier worker-based Substrate prototype used the provider router and Actor DNS route instead of transient worker Pod IPs. The current ACP RuntimePool path does not call a Substrate workspace daemon.

If an Actor-backed `orka.harness.v2` supervisor is implemented, it should still use the provider's stable authenticated route rather than provider-native Pod placement details. That routing choice must not weaken exact runtime-instance fencing or expose Git/provider credentials to the wrong process tree.

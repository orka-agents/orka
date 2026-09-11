# ADR 0004: Use a minimal Substrate control API client

## Status

Deferred with the Substrate ACP integration.

A future Actor-backed RuntimeSession provider should continue to generate or vendor only the narrow Substrate lifecycle API surface it needs, rather than importing the full Substrate module. Any checkpoint/restore client must remain capability-specific and must not imply ACP prompt replay, provider-session restore, or publication from an Actor-controlled workspace.

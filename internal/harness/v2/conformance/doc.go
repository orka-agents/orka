// Package conformance validates external orka.harness.v2 runtimes before an
// AgentRuntime becomes Ready. It probes safe and authenticated control surfaces,
// enforces exact immutable identity/profile/limit claims, and runs a bounded
// single-attempt hostile lifecycle cycle without prompt reconnect or replay.
package conformance

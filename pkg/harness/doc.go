// Package harness exposes the stable Orka harness protocol for external
// AgentRuntime adapters.
//
// Adapter implementations should depend on this package rather than Orka's
// internal controller packages. The wire contract remains versioned by
// ProtocolVersion.
package harness

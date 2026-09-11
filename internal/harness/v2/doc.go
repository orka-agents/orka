// Package v2 defines the portable orka.harness.v2 wire contract.
//
// The package intentionally contains protocol types, validation, canonical
// request digesting, replay/fence classification, lifecycle rules, and bounded
// NDJSON framing only. It does not implement a runtime supervisor, persistence,
// authentication transport, or provider process management.
package v2

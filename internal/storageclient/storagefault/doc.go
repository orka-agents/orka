// Package storagefault injects storage faults for tests. It wraps the
// providers behind Orka's storage client so a test can make a store
// unreachable and bring it back, slow calls down or hold them until released,
// lose a response after the provider committed, reject credentials, or change
// what Describe reports, and it counts every call it sees.
//
// An [Injector] holds the faults and the counts. Wrap the providers a test
// builds the storage client from with [Injector.WrapState],
// [Injector.WrapArtifacts] or [Injector.WrapEmbedded], or use
// [NewEmbeddedClient] for a complete client over one embedded store:
//
//	client, faults, err := storagefault.NewEmbeddedClient(sqliteStore)
//	outage := faults.Down(storagev1.StateContract) // state storage is unreachable
//	...
//	outage.Remove() // and back
//
// Every method of the provider interfaces and of the embedded families is
// wrapped, so a fault applies to every call that reaches the contract it
// names. Two limits follow from the embedded SQLite store. Calls that a scan
// ingestion makes through the transaction-scoped store it passes to its
// callback go straight to SQLite. And SQLite serves every call on one
// connection, so a call held or delayed inside a task-data transaction,
// which holds that connection, also blocks every other call, including
// HealthCheck. The methods of OperationReceiptStore and
// ControllerEpochAdvancer are not wrapped until they join the provider
// interfaces; a test fails if a provider interface gains a catalog method the
// wrappers miss.
//
// The wrappers are generated (wrappers_gen.go); run go generate in this
// package after a provider interface changes.
package storagefault

//go:generate go run ./internal/wrappergen -o wrappers_gen.go

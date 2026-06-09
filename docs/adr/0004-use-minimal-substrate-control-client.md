# Use a minimal Substrate control API client

Orka should generate or vendor only the small Substrate API surface needed for actor lifecycle operations instead of importing the full `github.com/agent-substrate/substrate` Go module. Orka needs narrow gRPC clients for the public control API and, when implementing checkpoint/restore features, the specific Ateom/AteomHerder checkpoint services. Avoiding the full module reduces Kubernetes dependency churn and cloud-provider transitive dependencies while still keeping capability-specific proto clients available inside `internal/substratepb`.

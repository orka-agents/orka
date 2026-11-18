//go:build linux

package main

type preparationIsolationFailure uint8

const (
	preparationIsolationIdentity preparationIsolationFailure = iota + 1
	preparationIsolationStatus
	preparationIsolationCapabilities
	preparationIsolationProcessSecurity
	preparationIsolationFilesystem
	preparationIsolationCredentials
)

func (e preparationIsolationFailure) Error() string {
	switch e {
	case preparationIsolationIdentity:
		return "dedicated worker identity is required"
	case preparationIsolationStatus:
		return "process isolation settings are unavailable"
	case preparationIsolationCapabilities:
		return "worker capabilities must be empty"
	case preparationIsolationProcessSecurity:
		return "no-new-privileges and RuntimeDefault are required"
	case preparationIsolationFilesystem:
		return "read-only image and bounded memory-backed workspace are required"
	case preparationIsolationCredentials:
		return "Kubernetes credentials must not be mounted"
	default:
		return "worker isolation policy is unavailable"
	}
}

func (e preparationIsolationFailure) PreparationIsolationReason() string {
	switch e {
	case preparationIsolationIdentity:
		return "identity"
	case preparationIsolationStatus:
		return "process-status"
	case preparationIsolationCapabilities:
		return "capabilities"
	case preparationIsolationProcessSecurity:
		return "process-security"
	case preparationIsolationFilesystem:
		return "filesystem"
	case preparationIsolationCredentials:
		return "credentials"
	default:
		return ""
	}
}

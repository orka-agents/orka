//go:build !unix

package acp

func hardenSupervisorPlatform() (HardeningStatus, error) {
	// Non-Unix platforms do not expose the process-wide umask or RLIMIT_CORE
	// controls used by the production Linux supervisor.
	return HardeningStatus{}, nil
}

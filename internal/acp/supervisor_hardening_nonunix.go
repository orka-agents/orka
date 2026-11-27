//go:build !unix

package acp

func hardenSupervisorPlatform() (HardeningStatus, error) {
	return HardeningStatus{}, nil
}

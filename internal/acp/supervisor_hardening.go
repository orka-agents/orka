package acp

import "fmt"

type HardeningStatus struct {
	Subreaper         bool
	CoreDumpsDisabled bool
	PrivateUmask      bool
}

func HardenSupervisorProcess() (HardeningStatus, error) {
	status, err := hardenSupervisorPlatform()
	if err != nil {
		return HardeningStatus{}, fmt.Errorf("harden ACP supervisor process: %w", err)
	}
	return status, nil
}

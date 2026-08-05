//go:build !unix

package publisher

import (
	"os/exec"
	"time"
)

func configureCommandCancellation(command *exec.Cmd) {
	command.WaitDelay = 2 * time.Second
}

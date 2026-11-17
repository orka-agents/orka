//go:build !linux

package supervisor

import (
	"encoding/json"
	"io"
)

// RunOperatorFileHelper is only supported on Linux.
func RunOperatorFileHelper(_ []string, _ io.Reader, stdout io.Writer) int {
	writeOperatorFileStatus(stdout, operatorFileStatus{Code: operatorFileCodeInvalid, Error: "operator file copies are only supported on Linux"})
	return 1
}

func writeOperatorFileStatus(stdout io.Writer, status operatorFileStatus) {
	raw, _ := json.Marshal(status)
	_, _ = stdout.Write(append(raw, '\n'))
}

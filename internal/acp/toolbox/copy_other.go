//go:build !unix

package toolbox

// Copy is only implemented for Unix runtime Pods.
func Copy(Options) (Summary, error) {
	return Summary{}, failf(ReasonUnsupported, "toolbox copy is supported only on Unix")
}

// CheckMounted is only implemented for Unix runtime Pods.
func CheckMounted(string, []string, string) error {
	return failf(ReasonUnsupported, "toolbox check is supported only on Unix")
}

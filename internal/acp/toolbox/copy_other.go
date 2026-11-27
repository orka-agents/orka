//go:build !unix

package toolbox

func Copy(Options) (Summary, error) {
	return Summary{}, failf(ReasonUnsupported, "toolbox copy is supported only on Unix")
}

func CheckMounted(string, []string, string) error {
	return failf(ReasonUnsupported, "toolbox check is supported only on Unix")
}

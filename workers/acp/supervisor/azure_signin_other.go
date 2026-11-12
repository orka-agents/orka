//go:build !linux

package supervisor

// asSessionChild runs fn as the supervisor: only Linux runtimes run session
// children under their own identities.
func asSessionChild(_, _ int, fn func() error) error {
	return fn()
}

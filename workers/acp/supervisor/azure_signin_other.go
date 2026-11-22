//go:build !linux

package supervisor

func asSessionChild(_, _ int, fn func() error) error {
	return fn()
}

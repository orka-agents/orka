//go:build !linux

package acp

// OperatorTagInUse is conservative outside Linux.
func OperatorTagInUse(_ int) bool { return false }

// KillOperatorTag is unsupported outside Linux.
func KillOperatorTag(_, _ int) bool { return false }

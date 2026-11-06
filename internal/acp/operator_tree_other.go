//go:build !linux

package acp

// OperatorTagInUse is conservative outside Linux: no tag can be proven free.
func OperatorTagInUse(_ int) bool { return true }

// KillOperatorTag is unsupported outside Linux.
func KillOperatorTag(_, _ int) bool { return false }

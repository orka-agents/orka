//go:build !linux

package acp

func OperatorTagInUse(_ int) bool { return true }

func KillOperatorTag(_, _ int) bool { return false }

func ReapOperatorTag(_, _ int) {}

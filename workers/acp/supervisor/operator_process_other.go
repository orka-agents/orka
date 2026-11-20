//go:build !linux

package supervisor

// waitOperatorLeaderExit is a no-op where operator commands cannot start.
func waitOperatorLeaderExit(int) {}

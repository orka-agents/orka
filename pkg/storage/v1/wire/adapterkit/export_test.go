package adapterkit

import "time"

// SetProbeTimeouts sets the probe server's read and idle timeouts for a test
// and returns a function that restores them.
func SetProbeTimeouts(read, idle time.Duration) (restore func()) {
	previousRead, previousIdle := probeReadTimeout, probeIdleTimeout
	probeReadTimeout, probeIdleTimeout = read, idle
	return func() { probeReadTimeout, probeIdleTimeout = previousRead, previousIdle }
}

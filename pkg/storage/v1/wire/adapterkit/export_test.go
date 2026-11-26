package adapterkit

import "time"

func SetProbeTimeouts(read, idle time.Duration) (restore func()) {
	previousRead, previousIdle := probeReadTimeout, probeIdleTimeout
	probeReadTimeout, probeIdleTimeout = read, idle
	return func() { probeReadTimeout, probeIdleTimeout = previousRead, previousIdle }
}

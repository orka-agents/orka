//go:build unix && !linux

package toolbox

import "golang.org/x/sys/unix"

// statMode and statDev normalize Stat_t field types across platforms (macOS
// uses narrower types than Linux).
func statMode(st *unix.Stat_t) uint32 { return uint32(st.Mode) }
func statDev(st *unix.Stat_t) uint64  { return uint64(st.Dev) } //nolint:gosec // Device numbers are identity, not arithmetic.

//go:build unix && !linux

package toolbox

import "golang.org/x/sys/unix"

func statMode(st *unix.Stat_t) uint32 { return uint32(st.Mode) }
func statDev(st *unix.Stat_t) uint64  { return uint64(st.Dev) } //nolint:gosec // Device numbers are identity, not arithmetic.

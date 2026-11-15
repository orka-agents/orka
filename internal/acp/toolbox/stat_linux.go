//go:build linux

package toolbox

import "golang.org/x/sys/unix"

// statMode and statDev normalize Stat_t field types across platforms; on
// Linux they already have the widest types.
func statMode(st *unix.Stat_t) uint32 { return st.Mode }
func statDev(st *unix.Stat_t) uint64  { return st.Dev }

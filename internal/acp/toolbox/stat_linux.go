//go:build linux

package toolbox

import "golang.org/x/sys/unix"

func statMode(st *unix.Stat_t) uint32 { return st.Mode }
func statDev(st *unix.Stat_t) uint64  { return st.Dev }

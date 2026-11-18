package patchverification

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWorkerSeccompDefaultDenyAndNetworkBounds(t *testing.T) {
	for _, platform := range []string{"linux/amd64", "linux/arm64"} {
		for _, network := range []string{Offline, LocalServices} {
			t.Run(platform+"/"+network, func(t *testing.T) {
				encoded, err := KubernetesSeccompProfile(platform, network)
				require.NoError(t, err)
				var profile seccompProfile
				require.NoError(t, json.Unmarshal(encoded, &profile))
				require.Equal(t, "SCMP_ACT_ERRNO", profile.DefaultAction)
				require.Equal(t, uint(1), profile.DefaultErrnoRet)
				families := []uint64{}
				for _, rule := range profile.Syscalls {
					for _, forbidden := range []string{"socketcall", "io_uring_setup", "unshare", "setns", "ptrace", "bpf", "mount"} {
						require.False(t, rule.Action == seccompAllow && slices.Contains(rule.Names, forbidden), forbidden)
					}
					if slices.Contains(rule.Names, "socket") {
						require.NotEmpty(t, rule.Args)
						require.Equal(t, uint(0), rule.Args[0].Index)
						require.Equal(t, seccompEqual, rule.Args[0].Op)
						if network == LocalServices {
							require.Len(t, rule.Args, 3)
							require.Equal(t, seccompArgument{Index: 1, Value: 15, ValueTwo: 1, Op: seccompMaskedEqual}, rule.Args[1])
							require.Equal(t, uint(2), rule.Args[2].Index)
							require.Equal(t, seccompEqual, rule.Args[2].Op)
							require.Contains(t, []uint64{0, 6}, rule.Args[2].Value)
						}
						families = append(families, rule.Args[0].Value)
					}
					if slices.Contains(rule.Names, "clone") {
						require.Equal(t, []seccompArgument{{Index: 0, Value: 0x7e020000, Op: seccompMaskedEqual}}, rule.Args)
					}
				}
				expected := []uint64{1}
				if network == LocalServices {
					expected = []uint64{2, 2, 10, 10}
				}
				require.Equal(t, expected, families)
				for name, index := range map[string]uint{"sendto": 3, "sendmsg": 2, "sendmmsg": 3} {
					matches := 0
					for _, rule := range profile.Syscalls {
						if rule.Action != seccompAllow || !slices.Contains(rule.Names, name) {
							continue
						}
						matches++
						var args []seccompArgument
						if network == LocalServices {
							args = []seccompArgument{{Index: index, Value: 0x20000000, Op: seccompMaskedEqual}}
						}
						require.Equal(t, args, rule.Args)
					}
					require.Equal(t, 1, matches)
				}
				matches := 0
				for _, rule := range profile.Syscalls {
					if rule.Action != seccompAllow || !slices.Contains(rule.Names, "socketpair") {
						continue
					}
					matches++
					args := []seccompArgument{{Index: 0, Value: 1, Op: seccompEqual}}
					if network == LocalServices {
						args = append(args, seccompArgument{Index: 1, Value: ^uint64(0x80000 | 0x800), ValueTwo: 1, Op: seccompMaskedEqual})
					}
					require.Equal(t, args, rule.Args)
				}
				require.Equal(t, 1, matches)
			})
		}
	}
	for _, platform := range []string{"", "linux/386", "darwin/arm64"} {
		_, err := KubernetesSeccompProfile(platform, Offline)
		require.Error(t, err)
	}
}

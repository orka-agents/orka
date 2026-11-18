package patchverification

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

const (
	seccompAllow       = "SCMP_ACT_ALLOW"
	seccompEqual       = "SCMP_CMP_EQ"
	seccompMaskedEqual = "SCMP_CMP_MASKED_EQ"
)

type seccompArgument struct {
	Index    uint   `json:"index"`
	Value    uint64 `json:"value"`
	ValueTwo uint64 `json:"valueTwo,omitempty"`
	Op       string `json:"op"`
}

type seccompRule struct {
	Names    []string          `json:"names"`
	Action   string            `json:"action"`
	ErrnoRet uint              `json:"errnoRet,omitempty"`
	Args     []seccompArgument `json:"args,omitempty"`
}

type seccompProfile struct {
	DefaultAction   string        `json:"defaultAction"`
	DefaultErrnoRet uint          `json:"defaultErrnoRet"`
	Architectures   []string      `json:"architectures"`
	Syscalls        []seccompRule `json:"syscalls"`
}

func workerSeccomp(platform, profile string) ([]byte, error) {
	architecture := map[string]string{"linux/amd64": "SCMP_ARCH_X86_64", "linux/arm64": "SCMP_ARCH_AARCH64"}[platform]
	if architecture == "" || (profile != Offline && profile != LocalServices) {
		return nil, errors.New("unsupported seccomp platform or network profile")
	}
	allowed := strings.Fields(`
		accept accept4 access alarm arch_prctl bind brk capget chdir chmod chown chown32
		clock_getres clock_gettime clock_nanosleep close close_range connect copy_file_range
		creat dup dup2 dup3 epoll_create epoll_create1 epoll_ctl epoll_pwait epoll_pwait2 epoll_wait
		eventfd eventfd2 execve execveat exit exit_group faccessat faccessat2 fadvise64 fallocate
		fchdir fchmod fchmodat fchmodat2 fchown fchown32 fchownat fcntl fcntl64 fdatasync
		flock fork fstat fstat64 fstatat64 fstatfs fstatfs64 fsync ftruncate ftruncate64
		futex futex_time64 getcpu getcwd getdents getdents64 getegid getegid32 geteuid geteuid32
		getgid getgid32 getgroups getgroups32 getitimer getpeername getpgid getpgrp getpid
		getppid getpriority getrandom getresgid getresgid32 getresuid getresuid32 getrlimit
		get_robust_list getrusage getsid getsockname getsockopt gettid gettimeofday getuid getuid32
		getxattr inotify_add_watch inotify_init inotify_init1 inotify_rm_watch ioctl kill
		lchown lchown32 lgetxattr link linkat listen listxattr llistxattr llseek lseek lstat lstat64
		madvise membarrier mincore mkdir mkdirat mlock mlock2 mlockall mmap mmap2 mprotect
		mremap msync munlock munlockall munmap nanosleep newfstatat open openat pause
		pipe pipe2 poll ppoll prctl pread64 preadv preadv2 prlimit64 pselect6 pwrite64 pwritev
		pwritev2 read readahead readlink readlinkat readv recvfrom recvmmsg recvmsg rename
		renameat renameat2 restart_syscall rmdir rseq rt_sigaction rt_sigpending rt_sigprocmask
		rt_sigqueueinfo rt_sigreturn rt_sigsuspend rt_sigtimedwait rt_tgsigqueueinfo
		sched_getaffinity sched_getattr sched_getparam sched_get_priority_max sched_get_priority_min
		sched_getscheduler sched_rr_get_interval sched_setaffinity sched_yield select sendfile
		sendfile64 sendmmsg sendmsg sendto setfsgid setfsgid32 setfsuid setfsuid32 setgid setgid32
		setgroups setgroups32 setitimer setpgid setpriority setregid setregid32 setresgid
		setresgid32 setresuid setresuid32 setreuid setreuid32 setrlimit setsid setsockopt
		set_tid_address setuid setuid32 set_robust_list shutdown sigaltstack signalfd signalfd4
		splice stat stat64 statfs statfs64 statx symlink symlinkat sync sync_file_range syncfs
		tee tgkill time timer_create timer_delete timer_getoverrun timer_gettime timer_settime
		timerfd_create timerfd_gettime timerfd_settime times tkill truncate truncate64 umask
		uname unlink unlinkat utime utimensat utimes vfork wait4 waitid waitpid write writev`)
	socketpairArguments := []seccompArgument{{Index: 0, Value: 1, Op: seccompEqual}}
	if profile == LocalServices {
		allowed = slices.DeleteFunc(allowed, func(name string) bool {
			return name == "sendto" || name == "sendmsg" || name == "sendmmsg"
		})
		socketpairArguments = append(socketpairArguments, seccompArgument{Index: 1, Value: ^uint64(0x80000 | 0x800), ValueTwo: 1, Op: seccompMaskedEqual})
	}
	rules := []seccompRule{
		{Names: allowed, Action: seccompAllow},
		{Names: []string{"clone"}, Action: seccompAllow, Args: []seccompArgument{{Index: 0, Value: 0x7e020000, Op: seccompMaskedEqual}}},
		{Names: []string{"clone3", "openat2"}, Action: "SCMP_ACT_ERRNO", ErrnoRet: 38},
		{Names: []string{"socketpair"}, Action: seccompAllow, Args: socketpairArguments},
	}
	if profile == Offline {
		rules = append(rules, seccompRule{Names: []string{"socket"}, Action: seccompAllow, Args: []seccompArgument{{Index: 0, Value: 1, Op: seccompEqual}}})
	} else {
		rules = append(rules, seccompRule{Names: []string{"landlock_create_ruleset", "landlock_add_rule", "landlock_restrict_self"}, Action: seccompAllow})
		for _, send := range []struct {
			name  string
			index uint
		}{{"sendto", 3}, {"sendmsg", 2}, {"sendmmsg", 3}} {
			rules = append(rules, seccompRule{Names: []string{send.name}, Action: seccompAllow, Args: []seccompArgument{{Index: send.index, Value: 0x20000000, Op: seccompMaskedEqual}}})
		}
		for _, family := range []uint64{2, 10} {
			for _, protocol := range []uint64{0, 6} {
				rules = append(rules, seccompRule{Names: []string{"socket"}, Action: seccompAllow, Args: []seccompArgument{
					{Index: 0, Value: family, Op: seccompEqual},
					{Index: 1, Value: 15, ValueTwo: 1, Op: seccompMaskedEqual},
					{Index: 2, Value: protocol, Op: seccompEqual},
				}})
			}
		}
	}
	return json.Marshal(seccompProfile{DefaultAction: "SCMP_ACT_ERRNO", DefaultErrnoRet: 1, Architectures: []string{architecture}, Syscalls: rules})
}

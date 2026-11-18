#define _GNU_SOURCE
#include <arpa/inet.h>
#include <errno.h>
#include <fcntl.h>
#include <grp.h>
#include <limits.h>
#include <linux/audit.h>
#include <linux/capability.h>
#include <linux/filter.h>
#include <linux/sched.h>
#include <linux/seccomp.h>
#include <stddef.h>
#include <stdint.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/prctl.h>
#include <sys/resource.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <unistd.h>

#if defined(__x86_64__)
#define NATIVE_ARCH AUDIT_ARCH_X86_64
#elif defined(__aarch64__)
#define NATIVE_ARCH AUDIT_ARCH_AARCH64
#else
#error "validation guard supports only native amd64 and arm64"
#endif

#define GUARD_READY "orka-validation-guard-v1\n"
#define FILTER_CAPACITY 1024
#define DENIED (SECCOMP_RET_ERRNO | EPERM)

struct policy {
    struct sock_filter instructions[FILTER_CAPACITY];
    size_t count;
};

static void emit(struct policy *policy, unsigned short code, unsigned char yes,
                 unsigned char no, unsigned int value) {
    if (policy->count >= FILTER_CAPACITY) {
        _exit(125);
    }
    policy->instructions[policy->count++] =
        (struct sock_filter){.code = code, .jt = yes, .jf = no, .k = value};
}

static void load_argument(struct policy *policy, unsigned int argument) {
    emit(policy, BPF_LD | BPF_W | BPF_ABS, 0, 0,
         (unsigned int)offsetof(struct seccomp_data, args) + argument * 8);
}

static void require_values(struct policy *policy, const unsigned int *values, size_t count) {
    for (size_t index = 0; index < count; ++index) {
        emit(policy, BPF_JMP | BPF_JEQ | BPF_K, (unsigned char)(count - index), 0, values[index]);
    }
    emit(policy, BPF_RET | BPF_K, 0, 0, DENIED);
}

static void require_value(struct policy *policy, unsigned int value) {
    require_values(policy, &value, 1);
}

static size_t begin_rule(struct policy *policy, unsigned int number) {
    size_t position = policy->count;
    emit(policy, BPF_JMP | BPF_JEQ | BPF_K, 0, 0, number);
    return position;
}

static void end_rule(struct policy *policy, size_t position) {
    size_t length = policy->count - position - 1;
    if (length > UCHAR_MAX) {
        _exit(125);
    }
    policy->instructions[position].jf = (unsigned char)length;
}

static void allow_call(struct policy *policy, unsigned int number) {
    emit(policy, BPF_JMP | BPF_JEQ | BPF_K, 0, 1, number);
    emit(policy, BPF_RET | BPF_K, 0, 0, SECCOMP_RET_ALLOW);
}

static void socket_rule(struct policy *policy, unsigned int number, int local, int pair) {
    size_t start = begin_rule(policy, number);
    load_argument(policy, 0);
    const unsigned int families[] = {AF_UNIX, AF_INET, AF_INET6};
    require_values(policy, families, local && !pair ? 3 : 1);
    load_argument(policy, 1);
    emit(policy, BPF_ALU | BPF_AND | BPF_K, 0, 0, ~(SOCK_NONBLOCK | SOCK_CLOEXEC));
    const unsigned int types[] = {SOCK_STREAM, SOCK_DGRAM, SOCK_SEQPACKET};
    require_values(policy, types, local || pair ? 1 : 3);
    load_argument(policy, 2);
    const unsigned int protocols[] = {0, IPPROTO_TCP};
    require_values(policy, protocols, local && !pair ? 2 : 1);
    emit(policy, BPF_RET | BPF_K, 0, 0, SECCOMP_RET_ALLOW);
    end_rule(policy, start);
}

static void accept_rule(struct policy *policy, unsigned int number) {
    size_t start = begin_rule(policy, number);
    load_argument(policy, 0);
    require_value(policy, 3);
    emit(policy, BPF_RET | BPF_K, 0, 0, SECCOMP_RET_ALLOW);
    end_rule(policy, start);
}

static void send_rule(struct policy *policy, unsigned int number, unsigned int flags_argument) {
    size_t start = begin_rule(policy, number);
    load_argument(policy, flags_argument);
    // TCP Fast Open can connect through sendmsg/sendto without connect(2).
    emit(policy, BPF_ALU | BPF_AND | BPF_K, 0, 0, MSG_FASTOPEN);
    require_value(policy, 0);
    emit(policy, BPF_RET | BPF_K, 0, 0, SECCOMP_RET_ALLOW);
    end_rule(policy, start);
}

static void build_policy(struct policy *policy, int local, int fixture) {
    memset(policy, 0, sizeof(*policy));
    emit(policy, BPF_LD | BPF_W | BPF_ABS, 0, 0, offsetof(struct seccomp_data, arch));
    emit(policy, BPF_JMP | BPF_JEQ | BPF_K, 1, 0, NATIVE_ARCH);
    emit(policy, BPF_RET | BPF_K, 0, 0, SECCOMP_RET_KILL_PROCESS);
    emit(policy, BPF_LD | BPF_W | BPF_ABS, 0, 0, offsetof(struct seccomp_data, nr));
#if defined(__x86_64__)
    emit(policy, BPF_JMP | BPF_JSET | BPF_K, 0, 1, 0x40000000U);
    emit(policy, BPF_RET | BPF_K, 0, 0, SECCOMP_RET_KILL_PROCESS);
#endif
    socket_rule(policy, __NR_socket, local, 0);
    socket_rule(policy, __NR_socketpair, local, 1);
    if (!fixture) {
        allow_call(policy, __NR_connect);
    }
    if (!local) {
        allow_call(policy, __NR_bind);
        allow_call(policy, __NR_listen);
        allow_call(policy, __NR_accept);
        allow_call(policy, __NR_accept4);
    } else if (fixture) {
        accept_rule(policy, __NR_accept);
        accept_rule(policy, __NR_accept4);
    }
    send_rule(policy, __NR_sendto, 3);
    send_rule(policy, __NR_sendmsg, 2);
    send_rule(policy, __NR_sendmmsg, 3);

    size_t clone = begin_rule(policy, __NR_clone);
    load_argument(policy, 0);
    emit(policy, BPF_ALU | BPF_AND | BPF_K, 0, 0,
         CLONE_NEWCGROUP | CLONE_NEWIPC | CLONE_NEWNET | CLONE_NEWNS |
         CLONE_NEWPID | CLONE_NEWUSER | CLONE_NEWUTS | CLONE_NEWTIME |
         CLONE_PARENT | CLONE_PTRACE | CLONE_UNTRACED);
    require_value(policy, 0);
    emit(policy, BPF_LD | BPF_W | BPF_ABS, 0, 0,
         (unsigned int)offsetof(struct seccomp_data, args) + 4);
    require_value(policy, 0);
    emit(policy, BPF_RET | BPF_K, 0, 0, SECCOMP_RET_ALLOW);
    end_rule(policy, clone);
#ifdef __NR_clone3
    emit(policy, BPF_JMP | BPF_JEQ | BPF_K, 0, 1, __NR_clone3);
    emit(policy, BPF_RET | BPF_K, 0, 0, SECCOMP_RET_ERRNO | ENOSYS);
#endif
#ifdef __NR_openat2
    emit(policy, BPF_JMP | BPF_JEQ | BPF_K, 0, 1, __NR_openat2);
    emit(policy, BPF_RET | BPF_K, 0, 0, SECCOMP_RET_ERRNO | ENOSYS);
#endif

#define ALLOW(name) allow_call(policy, __NR_##name)
    ALLOW(brk);
    ALLOW(capget);
    ALLOW(chdir);
    ALLOW(clock_getres);
    ALLOW(clock_gettime);
    ALLOW(clock_nanosleep);
    ALLOW(close);
#ifdef __NR_close_range
    ALLOW(close_range);
#endif
    ALLOW(copy_file_range);
    ALLOW(dup);
    ALLOW(dup3);
    ALLOW(epoll_create1);
    ALLOW(epoll_ctl);
    ALLOW(epoll_pwait);
#ifdef __NR_epoll_pwait2
    ALLOW(epoll_pwait2);
#endif
    ALLOW(eventfd2);
    ALLOW(execve);
    ALLOW(execveat);
    ALLOW(exit);
    ALLOW(exit_group);
    ALLOW(faccessat);
#ifdef __NR_faccessat2
    ALLOW(faccessat2);
#endif
    ALLOW(fadvise64);
    ALLOW(fallocate);
    ALLOW(fchdir);
    ALLOW(fchmod);
    ALLOW(fchmodat);
#ifdef __NR_fchmodat2
    ALLOW(fchmodat2);
#endif
    ALLOW(fcntl);
    ALLOW(fdatasync);
    ALLOW(flock);
    ALLOW(fstat);
    ALLOW(fstatfs);
    ALLOW(fsync);
    ALLOW(ftruncate);
    ALLOW(futex);
    ALLOW(getcpu);
    ALLOW(getcwd);
    ALLOW(getdents64);
    ALLOW(getegid);
    ALLOW(geteuid);
    ALLOW(getgid);
    ALLOW(getgroups);
    ALLOW(getitimer);
    ALLOW(getpeername);
    ALLOW(getpgid);
    ALLOW(getpid);
    ALLOW(getppid);
    ALLOW(getpriority);
    ALLOW(getrandom);
    ALLOW(getresgid);
    ALLOW(getresuid);
    ALLOW(getrlimit);
    ALLOW(get_robust_list);
    ALLOW(getrusage);
    ALLOW(getsid);
    ALLOW(getsockname);
    ALLOW(getsockopt);
    ALLOW(gettid);
    ALLOW(gettimeofday);
    ALLOW(getuid);
    ALLOW(getxattr);
    ALLOW(inotify_add_watch);
    ALLOW(inotify_init1);
    ALLOW(inotify_rm_watch);
    ALLOW(ioctl);
    ALLOW(kill);
    ALLOW(lgetxattr);
    ALLOW(linkat);
    ALLOW(listxattr);
    ALLOW(llistxattr);
    ALLOW(lseek);
    ALLOW(madvise);
    ALLOW(membarrier);
    ALLOW(mincore);
    ALLOW(mkdirat);
    ALLOW(mmap);
    ALLOW(mprotect);
    ALLOW(mremap);
    ALLOW(msync);
    ALLOW(munmap);
    ALLOW(nanosleep);
    ALLOW(newfstatat);
    ALLOW(openat);
    ALLOW(pipe2);
    ALLOW(ppoll);
    ALLOW(prctl);
    ALLOW(pread64);
    ALLOW(preadv);
    ALLOW(preadv2);
    ALLOW(prlimit64);
    ALLOW(pselect6);
    ALLOW(pwrite64);
    ALLOW(pwritev);
    ALLOW(pwritev2);
    ALLOW(read);
    ALLOW(readahead);
    ALLOW(readlinkat);
    ALLOW(readv);
    ALLOW(recvfrom);
    ALLOW(recvmmsg);
    ALLOW(recvmsg);
    ALLOW(renameat);
    ALLOW(renameat2);
    ALLOW(restart_syscall);
    ALLOW(rseq);
    ALLOW(rt_sigaction);
    ALLOW(rt_sigpending);
    ALLOW(rt_sigprocmask);
    ALLOW(rt_sigqueueinfo);
    ALLOW(rt_sigreturn);
    ALLOW(rt_sigsuspend);
    ALLOW(rt_sigtimedwait);
    ALLOW(rt_tgsigqueueinfo);
    ALLOW(sched_getaffinity);
    ALLOW(sched_getattr);
    ALLOW(sched_getparam);
    ALLOW(sched_get_priority_max);
    ALLOW(sched_get_priority_min);
    ALLOW(sched_getscheduler);
    ALLOW(sched_rr_get_interval);
    ALLOW(sched_setaffinity);
    ALLOW(sched_yield);
    ALLOW(sendfile);
    ALLOW(setitimer);
    ALLOW(setpgid);
    ALLOW(setpriority);
    ALLOW(setrlimit);
    ALLOW(setsid);
    ALLOW(setsockopt);
    ALLOW(set_tid_address);
    ALLOW(set_robust_list);
    ALLOW(shutdown);
    ALLOW(sigaltstack);
    ALLOW(signalfd4);
    ALLOW(splice);
    ALLOW(statfs);
    ALLOW(statx);
    ALLOW(symlinkat);
    ALLOW(sync_file_range);
    ALLOW(tee);
    ALLOW(tgkill);
    ALLOW(timer_create);
    ALLOW(timer_delete);
    ALLOW(timer_getoverrun);
    ALLOW(timer_gettime);
    ALLOW(timer_settime);
    ALLOW(timerfd_create);
    ALLOW(timerfd_gettime);
    ALLOW(timerfd_settime);
    ALLOW(times);
    ALLOW(tkill);
    ALLOW(truncate);
    ALLOW(umask);
    ALLOW(uname);
    ALLOW(unlinkat);
    ALLOW(utimensat);
    ALLOW(wait4);
    ALLOW(waitid);
    ALLOW(write);
    ALLOW(writev);
#ifdef __NR_access
    ALLOW(access);
#endif
#ifdef __NR_arch_prctl
    ALLOW(arch_prctl);
#endif
#ifdef __NR_chmod
    ALLOW(chmod);
#endif
#ifdef __NR_dup2
    ALLOW(dup2);
#endif
#ifdef __NR_epoll_create
    ALLOW(epoll_create);
#endif
#ifdef __NR_epoll_wait
    ALLOW(epoll_wait);
#endif
#ifdef __NR_eventfd
    ALLOW(eventfd);
#endif
#ifdef __NR_fork
    ALLOW(fork);
#endif
#ifdef __NR_getdents
    ALLOW(getdents);
#endif
#ifdef __NR_getpgrp
    ALLOW(getpgrp);
#endif
#ifdef __NR_inotify_init
    ALLOW(inotify_init);
#endif
#ifdef __NR_link
    ALLOW(link);
#endif
#ifdef __NR_lstat
    ALLOW(lstat);
#endif
#ifdef __NR_mkdir
    ALLOW(mkdir);
#endif
#ifdef __NR_open
    ALLOW(open);
#endif
#ifdef __NR_pause
    ALLOW(pause);
#endif
#ifdef __NR_pipe
    ALLOW(pipe);
#endif
#ifdef __NR_poll
    ALLOW(poll);
#endif
#ifdef __NR_readlink
    ALLOW(readlink);
#endif
#ifdef __NR_rename
    ALLOW(rename);
#endif
#ifdef __NR_rmdir
    ALLOW(rmdir);
#endif
#ifdef __NR_select
    ALLOW(select);
#endif
#ifdef __NR_signalfd
    ALLOW(signalfd);
#endif
#ifdef __NR_stat
    ALLOW(stat);
#endif
#ifdef __NR_symlink
    ALLOW(symlink);
#endif
#ifdef __NR_time
    ALLOW(time);
#endif
#ifdef __NR_unlink
    ALLOW(unlink);
#endif
#ifdef __NR_utime
    ALLOW(utime);
#endif
#ifdef __NR_utimes
    ALLOW(utimes);
#endif
#ifdef __NR_vfork
    ALLOW(vfork);
#endif
#undef ALLOW
    // No namespace, mount, ptrace, process_vm, pidfd_getfd, BPF, keyring,
    // userfaultfd, io_uring, device creation, or privilege-changing syscalls.
    emit(policy, BPF_RET | BPF_K, 0, 0, DENIED);
}

static int install_policy(int local, int fixture) {
    struct policy policy;
    build_policy(&policy, local, fixture);
    struct sock_fprog program = {.len = (unsigned short)policy.count, .filter = policy.instructions};
    if (prctl(PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) != 0 ||
        prctl(PR_SET_SECCOMP, SECCOMP_MODE_FILTER, &program) != 0) {
        return -1;
    }
    return 0;
}

static int bounded_limit(int resource, rlim_t ceiling) {
    struct rlimit limit;
    if (getrlimit(resource, &limit) != 0) {
        return -1;
    }
    if (limit.rlim_max > ceiling) {
        limit.rlim_max = ceiling;
    }
    limit.rlim_cur = limit.rlim_max;
    return setrlimit(resource, &limit);
}

static int install_resource_limits(void) {
    // NPROC is charged to the real UID across containers. The supervisor
    // verifies this container's finite pids.max before launching any role.
    return bounded_limit(RLIMIT_CORE, 0) != 0 ||
           bounded_limit(RLIMIT_NOFILE, 256) != 0 ||
           bounded_limit(RLIMIT_FSIZE, 256 * 1024 * 1024) != 0 ? -1 : 0;
}

static int close_inherited(int last) {
#ifdef __NR_close_range
    if (syscall(__NR_close_range, (unsigned int)last + 1, ~0U, 0) == 0) {
        return 0;
    }
    if (errno != ENOSYS && errno != EINVAL && errno != EPERM) {
        return -1;
    }
#endif
    struct rlimit limit;
    if (getrlimit(RLIMIT_NOFILE, &limit) != 0 || limit.rlim_max > 1048576) {
        return -1;
    }
    for (int descriptor = last + 1; (rlim_t)descriptor < limit.rlim_max; ++descriptor) {
        if (close(descriptor) != 0 && errno != EBADF) {
            return -1;
        }
    }
    return 0;
}

static int numeric(const char *value, unsigned long minimum, unsigned long maximum, unsigned long *out) {
    if (value == NULL || value[0] < '0' || value[0] > '9') {
        return -1;
    }
    errno = 0;
    char *end = NULL;
    unsigned long parsed = strtoul(value, &end, 10);
    if (errno != 0 || *end != '\0' || parsed < minimum || parsed > maximum) {
        return -1;
    }
    *out = parsed;
    return 0;
}

static int listener_valid(unsigned long port) {
    struct sockaddr_in6 address;
    socklen_t length = sizeof(address);
    int accepting = 0, only_v6 = 1, type = 0;
    socklen_t option_length = sizeof(int);
    memset(&address, 0, sizeof(address));
    if (getsockname(3, (struct sockaddr *)&address, &length) != 0 ||
        length != sizeof(address) || address.sin6_family != AF_INET6 ||
        ntohs(address.sin6_port) != port ||
        getsockopt(3, SOL_SOCKET, SO_TYPE, &type, &option_length) != 0 || type != SOCK_STREAM ||
        getsockopt(3, SOL_SOCKET, SO_ACCEPTCONN, &accepting, &option_length) != 0 || accepting != 1 ||
        getsockopt(3, IPPROTO_IPV6, IPV6_V6ONLY, &only_v6, &option_length) != 0) {
        return -1;
    }
    // Linux makes a socket bound to ::1 IPv6-only even after V6ONLY was
    // cleared. HTTP uses that exact loopback listener; legacy local fixtures
    // use a dual-stack wildcard listener. Accept no other binding shape.
    int loopback = IN6_IS_ADDR_LOOPBACK(&address.sin6_addr);
    int wildcard = IN6_IS_ADDR_UNSPECIFIED(&address.sin6_addr);
    if ((!loopback && !wildcard) || only_v6 != (loopback ? 1 : 0)) {
        return -1;
    }
    return fcntl(3, F_SETFD, 0);
}

static int drop_identity(uid_t uid) {
    struct __user_cap_header_struct header = {.version = _LINUX_CAPABILITY_VERSION_3, .pid = 0};
    struct __user_cap_data_struct capabilities[2] = {{0}, {0}};
    if (prctl(PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) != 0 ||
        prctl(PR_CAP_AMBIENT, PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0) != 0 ||
        setgroups(0, NULL) != 0 || setresgid(uid, uid, uid) != 0 || setresuid(uid, uid, uid) != 0 ||
        syscall(__NR_capset, &header, capabilities) != 0 ||
        syscall(__NR_capget, &header, capabilities) != 0 ||
        capabilities[0].effective != 0 || capabilities[0].permitted != 0 ||
        capabilities[0].inheritable != 0 || capabilities[1].effective != 0 ||
        capabilities[1].permitted != 0 || capabilities[1].inheritable != 0 ||
        getuid() != uid || geteuid() != uid || getgid() != uid || getegid() != uid ||
        getgroups(0, NULL) != 0 || prctl(PR_SET_DUMPABLE, 0, 0, 0, 0) != 0) {
        return -1;
    }
    return 0;
}

_Noreturn static void failed(int status) {
    if (status >= 0) {
        const char failure[] = "E";
        ssize_t ignored = write(status, failure, sizeof(failure) - 1);
        (void)ignored;
    }
    _exit(125);
}

int main(int argc, char **argv) {
    // Positional protocol: UID, profile, role, listener port (0 for subject), --, command...
    if (argc < 7) {
        failed(-1);
    }
    int fixture = strcmp(argv[3], "fixture") == 0;
    int status = fixture ? 4 : 3;
    pid_t parent = getppid();
    unsigned long uid = 0, port = 0;
    int local = strcmp(argv[2], "local-services") == 0;
    struct stat status_info;
    if ((!fixture && strcmp(argv[3], "subject") != 0) ||
        (!local && strcmp(argv[2], "offline") != 0) ||
        (fixture && !local) || strcmp(argv[5], "--") != 0 ||
        numeric(argv[1], 20000, 60000, &uid) != 0 ||
        numeric(argv[4], fixture ? 1024 : 0, fixture ? 65535 : 0, &port) != 0 ||
        getuid() != 0 || geteuid() != 0 ||
        fstat(status, &status_info) != 0 || !S_ISFIFO(status_info.st_mode) ||
        fcntl(status, F_SETFD, FD_CLOEXEC) != 0 ||
        close_inherited(status) != 0 ||
        (fixture && listener_valid(port) != 0) ||
        unsetenv("ORKA_LISTEN_FD") != 0 ||
        (fixture && setenv("ORKA_LISTEN_FD", "3", 1) != 0) ||
        install_resource_limits() != 0) {
        failed(status);
    }
    umask(0077);
    char work[64];
    int length = snprintf(work, sizeof(work), "/work/%lu", uid);
    // A root observer without CAP_DAC_OVERRIDE cannot chdir into the role's
    // 0700 tree. Enter it only after dropping to that exact role.
    if (length <= 0 || (size_t)length >= sizeof(work) ||
        drop_identity((uid_t)uid) != 0 || chdir(work) != 0 ||
        prctl(PR_SET_PDEATHSIG, SIGKILL, 0, 0, 0) != 0 || getppid() != parent ||
        install_policy(local, fixture) != 0) {
        failed(status);
    }
    if (write(status, GUARD_READY, sizeof(GUARD_READY) - 1) != (ssize_t)sizeof(GUARD_READY) - 1) {
        failed(status);
    }
    execv(argv[6], &argv[6]);
    failed(status);
}

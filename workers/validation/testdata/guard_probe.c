#define main validation_guard_main
#include "../guard/guard.c"
#undef main

#include <signal.h>
#include <stdio.h>
#include <sys/ptrace.h>
#include <sys/uio.h>
#include <sys/wait.h>

#define REQUIRE(condition) do { if (!(condition)) { return __LINE__ % 100 + 1; } } while (0)
#define DENY(expression) do { errno = 0; long result = (long)(expression); REQUIRE(result == -1 && errno == EPERM); } while (0)

static int common_denials(void) {
    DENY(socket(AF_PACKET, SOCK_RAW, 0));
    DENY(syscall(__NR_unshare, CLONE_NEWUSER));
    DENY(syscall(__NR_setns, -1, CLONE_NEWNET));
    DENY(syscall(__NR_ptrace, PTRACE_TRACEME, 0, 0, 0));
    DENY(syscall(__NR_process_vm_writev, getpid(), NULL, 0, NULL, 0, 0));
    DENY(syscall(__NR_mount, NULL, NULL, NULL, 0, NULL));
    DENY(syscall(__NR_clone, CLONE_NEWNET | SIGCHLD, NULL, NULL, NULL, 0));
#ifdef __NR_io_uring_setup
    DENY(syscall(__NR_io_uring_setup, 1, NULL));
#endif
#ifdef __NR_pidfd_getfd
    DENY(syscall(__NR_pidfd_getfd, -1, 0, 0));
#endif
#ifdef __NR_bpf
    DENY(syscall(__NR_bpf, 0, NULL, 0));
#endif
#ifdef __NR_clone3
    errno = 0;
    REQUIRE(syscall(__NR_clone3, NULL, 0) == -1 && errno == ENOSYS);
#endif
    return 0;
}

static int local_checks(int fixture, unsigned long port) {
    int stream = socket(AF_INET, SOCK_STREAM, IPPROTO_TCP);
    REQUIRE(stream >= 0);
    struct sockaddr_in endpoint = {.sin_family = AF_INET, .sin_port = htons((uint16_t)port)};
    endpoint.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    DENY(bind(stream, (struct sockaddr *)&endpoint, sizeof(endpoint)));
    DENY(listen(stream, 1));
    DENY(socket(AF_INET, SOCK_DGRAM, 0));
    DENY(socket(AF_INET6, SOCK_DGRAM, 0));
    DENY(socket(AF_INET, SOCK_RAW, IPPROTO_TCP));
    DENY(socket(AF_UNIX, SOCK_DGRAM, 0));
    struct iovec vector = {.iov_base = (void *)"x", .iov_len = 1};
    struct msghdr message = {.msg_name = &endpoint, .msg_namelen = sizeof(endpoint),
                            .msg_iov = &vector, .msg_iovlen = 1};
    DENY(sendmsg(stream, &message, MSG_FASTOPEN));
    DENY(sendto(stream, "x", 1, MSG_FASTOPEN, (struct sockaddr *)&endpoint, sizeof(endpoint)));
    if (fixture) {
        DENY(connect(stream, (struct sockaddr *)&endpoint, sizeof(endpoint)));
        int duplicate = dup(3);
        REQUIRE(duplicate >= 0);
        DENY(accept(duplicate, NULL, NULL));
        REQUIRE(close(duplicate) == 0);
        int connection = accept4(3, NULL, NULL, SOCK_CLOEXEC);
        REQUIRE(connection >= 0);
        char input = 0;
        REQUIRE(read(connection, &input, 1) == 1 && input == 'x');
        REQUIRE(write(connection, "y", 1) == 1);
        REQUIRE(close(connection) == 0);
    } else {
        DENY(accept(stream, NULL, NULL));
        REQUIRE(connect(stream, (struct sockaddr *)&endpoint, sizeof(endpoint)) == 0);
        REQUIRE(write(stream, "s", 1) == 1);
    }
    REQUIRE(close(stream) == 0);
    return common_denials();
}

int main(int argc, char **argv) {
    REQUIRE(argc >= 2);
    struct rlimit processes_before, processes_after;
    REQUIRE(getrlimit(RLIMIT_NPROC, &processes_before) == 0);
    REQUIRE(install_resource_limits() == 0);
    REQUIRE(getrlimit(RLIMIT_NPROC, &processes_after) == 0);
    REQUIRE(processes_before.rlim_cur == processes_after.rlim_cur &&
            processes_before.rlim_max == processes_after.rlim_max);
    if (strcmp(argv[1], "listener-check") == 0) {
        unsigned long port = 0;
        REQUIRE(argc == 3 && numeric(argv[2], 1024, 65535, &port) == 0);
        return listener_valid(port) == 0 ? 0 : 125;
    }
    if (strcmp(argv[1], "fds-after-exec") == 0) {
        for (int descriptor = 3; descriptor < 16; ++descriptor) {
            errno = 0;
            REQUIRE(fcntl(descriptor, F_GETFD) == -1 && errno == EBADF);
        }
        REQUIRE(write(1, "closed\n", 7) == 7);
        return 0;
    }
    if (strcmp(argv[1], "fds") == 0) {
        REQUIRE(fcntl(3, F_SETFD, FD_CLOEXEC) == 0);
        REQUIRE(close_inherited(3) == 0);
        char *arguments[] = {argv[0], "fds-after-exec", NULL};
        execv(argv[0], arguments);
        return 100;
    }
    int local = strcmp(argv[1], "subject") == 0 || strcmp(argv[1], "fixture") == 0;
    int fixture = strcmp(argv[1], "fixture") == 0;
    REQUIRE(install_policy(local, fixture) == 0);
    REQUIRE(prctl(PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0) == 1);
    REQUIRE(prctl(PR_GET_SECCOMP, 0, 0, 0, 0) == SECCOMP_MODE_FILTER);
    if (strcmp(argv[1], "exec") == 0) {
        REQUIRE(argc >= 3);
        execv(argv[2], &argv[2]);
        return 125;
    }
    if (strcmp(argv[1], "x32") == 0) {
#if defined(__x86_64__)
        (void)syscall(__NR_getpid | 0x40000000U);
#endif
        return 100;
    }
    if (local) {
        unsigned long port = 0;
        REQUIRE(argc == 3 && numeric(argv[2], 1, 65535, &port) == 0);
        if (fixture) {
            REQUIRE(listener_valid(port) == 0);
        }
        return local_checks(fixture, port);
    }
    DENY(socket(AF_INET, SOCK_STREAM, 0));
    DENY(socket(AF_INET6, SOCK_STREAM, 0));
    int sockets[2];
    REQUIRE(socketpair(AF_UNIX, SOCK_STREAM, 0, sockets) == 0);
    REQUIRE(write(sockets[0], "a", 1) == 1);
    char input = 0;
    REQUIRE(read(sockets[1], &input, 1) == 1 && input == 'a');
    REQUIRE(close(sockets[0]) == 0 && close(sockets[1]) == 0);
    REQUIRE(common_denials() == 0);
    char *arguments[] = {"/bin/sh", "-c", "printf 'guard-inherited\\n'; exit 0", NULL};
    execv(arguments[0], arguments);
    return 100;
}

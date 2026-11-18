#define _GNU_SOURCE
#include <errno.h>
#include <limits.h>
#include <linux/audit.h>
#include <linux/filter.h>
#include <linux/landlock.h>
#include <linux/seccomp.h>
#include <netinet/in.h>
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <sys/prctl.h>
#include <sys/socket.h>
#include <sys/syscall.h>
#include <unistd.h>

#if defined(__x86_64__)
#define NATIVE_ARCH AUDIT_ARCH_X86_64
#elif defined(__aarch64__)
#define NATIVE_ARCH AUDIT_ARCH_AARCH64
#else
#error unsupported native architecture
#endif

static int unavailable(void)
{
    static const char message[] = "local TCP confinement unavailable\n";
    (void)write(STDERR_FILENO, message, sizeof(message) - 1);
    return 125;
}

static bool allow_ports(int ruleset, const char *ports, uint64_t access)
{
    if (strcmp(ports, "-") == 0)
        return true;
    unsigned int count = 0;
    while (*ports != '\0') {
        if (++count > 8 || *ports < '0' || *ports > '9')
            return false;
        char *end = NULL;
        errno = 0;
        unsigned long port = strtoul(ports, &end, 10);
        if (errno != 0 || end - ports > 5 || port < 1024 || port > 65535 ||
            (*end != '\0' && *end != ','))
            return false;
        struct landlock_net_port_attr rule = {
            .allowed_access = access,
            .port = port,
        };
        if (syscall(SYS_landlock_add_rule, ruleset, LANDLOCK_RULE_NET_PORT, &rule, 0) != 0)
            return false;
        if (*end == '\0')
            return true;
        ports = end + 1;
    }
    return false;
}

static bool fixture_port(const char *text, uint16_t *port)
{
    if (strcmp(text, "-") == 0) {
        *port = 0;
        return true;
    }
    if (*text < '0' || *text > '9')
        return false;
    char *end = NULL;
    errno = 0;
    unsigned long parsed = strtoul(text, &end, 10);
    if (errno != 0 || end - text > 5 || *end != '\0' || parsed < 1024 || parsed > 65535)
        return false;
    *port = (uint16_t)parsed;
    return true;
}

static bool activate_listener(uint16_t port)
{
    int listener = socket(AF_INET6, SOCK_STREAM, IPPROTO_TCP);
    if (listener < 0)
        return false;
    int v6only = 0;
    struct sockaddr_in6 address = {
        .sin6_family = AF_INET6,
        .sin6_port = htons(port),
        .sin6_addr = IN6ADDR_ANY_INIT,
    };
    if (listener != 3 ||
        setsockopt(listener, IPPROTO_IPV6, IPV6_V6ONLY, &v6only, sizeof(v6only)) != 0 ||
        bind(listener, (struct sockaddr *)&address, sizeof(address)) != 0 ||
        listen(listener, 16) != 0 || setenv("ORKA_LISTEN_FD", "3", 1) != 0) {
        (void)close(listener);
        return false;
    }
    return true;
}

static bool deny_listeners(void)
{
    struct sock_filter filter[] = {
        BPF_STMT(BPF_LD | BPF_W | BPF_ABS, offsetof(struct seccomp_data, arch)),
        BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, NATIVE_ARCH, 1, 0),
        BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_KILL_PROCESS),
        BPF_STMT(BPF_LD | BPF_W | BPF_ABS, offsetof(struct seccomp_data, nr)),
#if defined(__x86_64__)
        BPF_JUMP(BPF_JMP | BPF_JSET | BPF_K, 0x40000000, 0, 1),
        BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_KILL_PROCESS),
#endif
        BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, SYS_bind, 1, 0),
        BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, SYS_listen, 0, 1),
        BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ERRNO | EPERM),
        BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ALLOW),
    };
    struct sock_fprog program = {
        .len = sizeof(filter) / sizeof(filter[0]),
        .filter = filter,
    };
    return prctl(PR_SET_SECCOMP, SECCOMP_MODE_FILTER, &program, 0, 0) == 0;
}

int main(int argc, char **argv)
{
    bool anchor = argc == 2 && strcmp(argv[1], "--anchor") == 0;
    if (!anchor && (argc < 7 || strcmp(argv[1], "--bind-tcp") != 0 ||
                   strcmp(argv[3], "--connect-tcp") != 0 || strcmp(argv[5], "--") != 0))
        return unavailable();
    uint16_t port = 0;
    if (!anchor && (!fixture_port(argv[2], &port) || (port != 0 && strcmp(argv[4], "-") != 0)))
        return unavailable();
    if (syscall(SYS_landlock_create_ruleset, NULL, 0, LANDLOCK_CREATE_RULESET_VERSION) < 4 ||
        syscall(SYS_close_range, 3, UINT_MAX, 0) != 0 ||
        prctl(PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) != 0 || unsetenv("ORKA_LISTEN_FD") != 0)
        return unavailable();
    struct landlock_ruleset_attr policy = {
        .handled_access_net = LANDLOCK_ACCESS_NET_BIND_TCP | LANDLOCK_ACCESS_NET_CONNECT_TCP,
    };
    int ruleset = (int)syscall(SYS_landlock_create_ruleset, &policy, sizeof(policy), 0);
    if (ruleset < 0)
        return unavailable();
    if ((!anchor && (!allow_ports(ruleset, argv[2], LANDLOCK_ACCESS_NET_BIND_TCP) ||
                     !allow_ports(ruleset, argv[4], LANDLOCK_ACCESS_NET_CONNECT_TCP))) ||
        syscall(SYS_landlock_restrict_self, ruleset, 0) != 0 || close(ruleset) != 0)
        return unavailable();
    if (port != 0 && !activate_listener(port)) {
        static const char message[] = "local TCP socket activation requires an available dual-stack IPv6 fixture port\n";
        (void)write(STDERR_FILENO, message, sizeof(message) - 1);
        return 125;
    }
    if (!deny_listeners())
        return unavailable();
    if (anchor) {
        static const char ready[] = "landlock-tcp-ready\n";
        if (write(STDOUT_FILENO, ready, sizeof(ready) - 1) != sizeof(ready) - 1)
            return unavailable();
        for (;;)
            pause();
    }
    execv(argv[6], &argv[6]);
    return unavailable();
}

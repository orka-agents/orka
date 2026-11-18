#define _GNU_SOURCE
#include <string.h>
#include <sys/prctl.h>
#include <sys/resource.h>
#include <unistd.h>

static int unavailable(void) {
    static const char message[] = "validation Git launcher unavailable\n";
    if (write(STDERR_FILENO, message, sizeof(message) - 1) < 0) {
        return 125;
    }
    return 125;
}

int main(int argc, char **argv) {
    if (argc < 3 || strcmp(argv[1], "/usr/bin/git") != 0) {
        return unavailable();
    }
    const struct {
        int resource;
        rlim_t value;
    } limits[] = {
        {RLIMIT_AS, 512UL << 20},
        {RLIMIT_CPU, 15},
        {RLIMIT_FSIZE, 64UL << 20},
        {RLIMIT_NOFILE, 128},
        {RLIMIT_CORE, 0},
    };
    /* This single-threaded native process, not the Go runtime, owns the
       post-limit interval. No argument or environment allocation follows. */
    for (unsigned int i = 0; i < sizeof(limits) / sizeof(limits[0]); i++) {
        const struct rlimit limit = {limits[i].value, limits[i].value};
        if (setrlimit(limits[i].resource, &limit) != 0) {
            return unavailable();
        }
    }
    if (prctl(PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) != 0) {
        return unavailable();
    }
    execv("/usr/bin/git", &argv[1]);
    return unavailable();
}

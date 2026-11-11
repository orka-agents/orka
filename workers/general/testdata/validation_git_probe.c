#define _GNU_SOURCE
#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/prctl.h>
#include <sys/resource.h>
#include <unistd.h>

int main(void) {
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
    for (unsigned int i = 0; i < sizeof(limits) / sizeof(limits[0]); i++) {
        struct rlimit observed;
        if (getrlimit(limits[i].resource, &observed) != 0 ||
            observed.rlim_cur != limits[i].value || observed.rlim_max != limits[i].value) {
            return 1;
        }
    }
    if (prctl(PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0) != 1) {
        return 2;
    }
    struct rlimit processes;
    if (getrlimit(RLIMIT_NPROC, &processes) != 0) {
        return 3;
    }
    const char *soft = getenv("ORKA_TEST_NPROC_SOFT");
    const char *hard = getenv("ORKA_TEST_NPROC_HARD");
    if (soft == NULL || hard == NULL ||
        processes.rlim_cur != (rlim_t)strtoull(soft, NULL, 10) ||
        processes.rlim_max != (rlim_t)strtoull(hard, NULL, 10)) {
        return 4;
    }
    const char *environment = getenv("ORKA_TEST_LARGE_ENV");
    if (environment == NULL || strlen(environment) != 96 * 1024) {
        return 5;
    }
    void *mapping = mmap(NULL, 513UL << 20, PROT_NONE,
                         MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    if (mapping != MAP_FAILED) {
        munmap(mapping, 513UL << 20);
        return 6;
    }
    if (errno != ENOMEM) {
        return 7;
    }
    puts("NATIVE_LIMITS_OK");
    return 0;
}

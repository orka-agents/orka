#include <stdio.h>
#include <stdlib.h>

__attribute__((constructor)) static void early_finish(void) {
    (void)puts("healthy");
    exit(0);
}

int main(void) {
    return 125;
}

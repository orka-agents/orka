#define main validation_guard_main
#include "../guard/guard.c"
#undef main

#include <signal.h>

static void stop_server(int signal_number) {
    (void)signal_number;
    _exit(0);
}

int main(int argc, char **argv) {
    const char response[] =
        "HTTP/1.1 200 OK\r\nContent-Length: 7\r\nConnection: close\r\n\r\nhealthy";
    if (signal(SIGTERM, stop_server) == SIG_ERR || install_policy(1, 1) != 0) {
        return 125;
    }
    errno = 0;
    if (connect(3, NULL, 0) != -1 || errno != EPERM ||
        write(4, GUARD_READY, sizeof(GUARD_READY) - 1) != (ssize_t)sizeof(GUARD_READY) - 1 ||
        close(4) != 0) {
        return 125;
    }
    int listener = 3;
    if (argc > 1 && strcmp(argv[1], "duplicate-listener") == 0) {
        listener = dup(3);
        if (listener < 0) {
            return 125;
        }
    }
    for (;;) {
        int connection = accept4(listener, NULL, NULL, SOCK_CLOEXEC);
        if (connection < 0) {
            return 125;
        }
        char request[4096] = {0};
        size_t received = 0;
        while (strstr(request, "\r\n\r\n") == NULL) {
            if (received == sizeof(request) - 1) {
                return 125;
            }
            ssize_t count = read(connection, request + received, sizeof(request) - received - 1);
            if (count <= 0) {
                return 125;
            }
            received += (size_t)count;
        }
        size_t sent = 0;
        while (sent < sizeof(response) - 1) {
            ssize_t count = write(connection, response + sent, sizeof(response) - 1 - sent);
            if (count <= 0) {
                return 125;
            }
            sent += (size_t)count;
        }
        if (close(connection) != 0) {
            return 125;
        }
    }
}

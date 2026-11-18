#define _POSIX_C_SOURCE 200809L
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <unistd.h>

static void stop_server(int signal_number) {
    (void)signal_number;
    _exit(0);
}

int main(void) {
    if (signal(SIGTERM, stop_server) == SIG_ERR) {
        return 125;
    }
    for (;;) {
        int client = accept(3, NULL, NULL);
        if (client < 0) {
            return 125;
        }
        char request[4096] = {0};
        size_t used = 0;
        while (strstr(request, "\r\n\r\n") == NULL) {
            ssize_t count = read(client, request + used, sizeof(request) - used - 1);
            if (count <= 0) {
                return 125;
            }
            used += (size_t)count;
            if (used == sizeof(request) - 1) {
                return 125;
            }
        }
        FILE *source = fopen("/src/README", "rb");
        if (source == NULL) {
            return 125;
        }
        char body[1024];
        size_t length = fread(body, 1, sizeof(body), source);
        if (ferror(source) || fclose(source) != 0) {
            return 125;
        }
        char response[2048];
        int header = snprintf(response, sizeof(response),
            "HTTP/1.1 200 OK\r\nConnection: close\r\nContent-Length: %zu\r\n\r\n", length);
        if (header < 0 || (size_t)header + length > sizeof(response)) {
            return 125;
        }
        memcpy(response + header, body, length);
        size_t written = 0;
        size_t total = (size_t)header + length;
        while (written < total) {
            ssize_t count = write(client, response + written, total - written);
            if (count <= 0) {
                return 125;
            }
            written += (size_t)count;
        }
        if (close(client) != 0) {
            return 125;
        }
    }
}

# syntax=docker/dockerfile:1.7.1@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e
# A deliberately hostile toolbox: a setuid-root binary, a binary with file
# capabilities, and a 0666 device node. Built in place (no scratch COPY) so the
# setuid bit, the security.capability xattr, and the device node survive. In
# copy mode the device node fails the copy (TOOLBOX_UNSUPPORTED_FILE_TYPE);
# with image volumes everything mounts as-is and must stay harmless: the
# setuid bit grants nothing under NoNewPrivs, the file-capability binary
# cannot run, and the device node cannot be opened.
FROM docker.io/library/debian:trixie-slim@sha256:020c0d20b9880058cbe785a9db107156c3c75c2ac944a6aa7ab59f2add76a7bd
RUN set -eu; \
    apt-get update; \
    apt-get install -y --no-install-recommends busybox-static libcap2-bin; \
    rm -rf /var/lib/apt/lists/*; \
    mkdir -p /opt/hostile/bin /opt/hostile/dev; \
    cp /bin/busybox /opt/hostile/bin/suid-busybox; \
    chown 0:0 /opt/hostile/bin/suid-busybox; \
    chmod 4755 /opt/hostile/bin/suid-busybox; \
    cp /bin/busybox /opt/hostile/bin/cap-busybox; \
    chmod 0755 /opt/hostile/bin/cap-busybox; \
    setcap cap_net_raw+ep /opt/hostile/bin/cap-busybox; \
    getcap /opt/hostile/bin/cap-busybox; \
    mknod -m 0666 /opt/hostile/dev/mem c 1 1; \
    ls -l /opt/hostile/bin /opt/hostile/dev

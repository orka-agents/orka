# syntax=docker/dockerfile:1.7.1@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e
# A toolbox folder that contains a FIFO; the copier must fail with
# TOOLBOX_UNSUPPORTED_FILE_TYPE instead of copying or opening it.
FROM docker.io/library/busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e
RUN mkdir -p /opt/fifo-tool/bin && printf '#!/bin/sh\necho fifo-tool\n' >/opt/fifo-tool/bin/fifo-tool && \
    chmod 0755 /opt/fifo-tool/bin/fifo-tool && mkfifo /opt/fifo-tool/pipe

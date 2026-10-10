# Hyperlight bundle: hluk (hyperlight-unikraft) and the guest images Orka runs
# code in, under /opt/orka/hyperlight. A Pod that runs Hyperlight micro-VMs
# copies it in with an init container (controller --hyperlight-bundle-image).

# hluk, built against Debian bookworm's glibc 2.36 so it runs on every Orka
# worker and ACP runtime base. The pinned commit carries the guest fixes Orka
# depends on: standard error kept apart, writes over 4 KiB whole, newlines as
# written, and pipes and $(...) in the shell (a vfork child on its parent's TLS).
FROM rust:1-bookworm@sha256:114c7a4425406451c2866b6aafe69fe29b1b298832db1277d411ac73c82d04d6 AS hluk
ARG HLUK_REPO=https://github.com/sozercan/hyperlight-unikraft.git
ARG HLUK_COMMIT=b588756661c42c102a2183a0d0daaef1c42c8d40
# The default build drives KVM (/dev/kvm). Nodes with only the Microsoft
# Hypervisor (/dev/mshv) need HLUK_FEATURES=mshv, which hluk leaves off by
# default because it gives up KVM's fast snapshot restore.
ARG HLUK_FEATURES=
WORKDIR /src
# The embedded kernel is committed, so the kernel submodules are not needed.
RUN git init -q . && git remote add origin "$HLUK_REPO" && \
    git fetch -q --depth 1 origin "$HLUK_COMMIT" && git checkout -q FETCH_HEAD
RUN cargo build --release --locked --bin hluk ${HLUK_FEATURES:+--features "$HLUK_FEATURES"} && \
    install -D -m 0755 target/release/hluk /out/bin/hluk

FROM debian:bookworm-slim@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587
COPY --from=hluk /out/bin/hluk /opt/orka/hyperlight/bin/hluk
# The published v0.17.0 guest images (initrd flavor: one /initrd.cpio), by digest.
COPY --from=ghcr.io/hyperlight-dev/hyperlight-unikraft/python@sha256:5c951e6351e0473af64a40c5d6d2b002796a65618a4f915787935a8a70ac8517 /initrd.cpio /opt/orka/hyperlight/rootfs/python.cpio
COPY --from=ghcr.io/hyperlight-dev/hyperlight-unikraft/node@sha256:cac9a0f5e0f45a90b60a23fd5e2c18180e336cf8a6c6e1b71e33ab87757b13c3 /initrd.cpio /opt/orka/hyperlight/rootfs/node.cpio
COPY --from=ghcr.io/hyperlight-dev/hyperlight-unikraft/bash@sha256:e0eab29a3ca2169760ad76f7137d84c576cf1b5e5d38cab62533ac73f083962e /initrd.cpio /opt/orka/hyperlight/rootfs/bash.cpio
USER 65532
LABEL org.opencontainers.image.title="Orka Hyperlight bundle" \
      org.opencontainers.image.description="hluk and its python, node and bash guest images for Hyperlight micro-VMs"

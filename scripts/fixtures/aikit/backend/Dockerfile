# CPU-only correction for the existing prebuilt Qwen image. No model/download changes.
FROM quay.io/go-skynet/ci-cache:base-grpc-amd64@sha256:3ec2f7c760378f174bd7c7e7d754715798d0abda10b93a1b25b7d25b69f9f7d1 AS builder
ENV BUILD_TYPE="" TARGETARCH=amd64
WORKDIR /LocalAI
COPY . .
RUN apt-get update -qq && apt-get install -y --no-install-recommends python3 && \
    cp -a /opt/grpc/. /usr/local/ && \
    make -C backend/cpp/llama-cpp llama.cpp/tools/grpc-server
RUN python3 orka-checkpoint-patch.py /LocalAI && \
    make -C backend/cpp/llama-cpp grpc-server SHARED_LIBS=ON \
      EXTRA_CMAKE_ARGS="-DGGML_BACKEND_DL=ON -DGGML_CPU_ALL_VARIANTS=ON" \
      TARGET="--target grpc-server --target ggml" JOBS=8 && \
    cp backend/cpp/llama-cpp/grpc-server backend/cpp/llama-cpp/llama-cpp-cpu-all && \
    mkdir -p backend/cpp/llama-cpp/ggml-shared-libs && \
    find backend/cpp/llama-cpp/llama.cpp/build -name '*.so*' \
      -exec cp -a {} backend/cpp/llama-cpp/ggml-shared-libs/ \; && \
    make -C backend/cpp/llama-cpp package

FROM ghcr.io/kaito-project/aikit/qwen3.5:4b@sha256:525dfb8b5ccc1c180f0eab633bcf459d2a574c14bfab01c03535191c810d121d
COPY --from=builder /LocalAI/backend/cpp/llama-cpp/package/ /backends/cpu-llama-cpp/

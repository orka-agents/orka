FROM --platform=$BUILDPLATFORM golang:1.26 AS builder

ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} go build -a -o /out/worker ./workers/agent/codex

RUN cat >/tmp/sandbox-runtime.go <<'GO'
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const appRoot = "/app"
const maxExecuteOutputBytes = 4 << 20

type executeRequest struct {
	Command string `json:"command"`
}

type executeResponse struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

type listEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

func main() {
	if err := os.MkdirAll(appRoot, 0o755); err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", health)
	mux.HandleFunc("/execute", execute)
	mux.HandleFunc("/upload", upload)
	mux.HandleFunc("/download/", download)
	mux.HandleFunc("/list/", list)
	mux.HandleFunc("/exists/", exists)
	if err := http.ListenAndServe(":8888", mux); err != nil {
		panic(err)
	}
}

func health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]string{"status": "ok"})
}

func execute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req executeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, executeResponse{Stderr: err.Error(), ExitCode: 1})
		return
	}
	cmd := exec.CommandContext(r.Context(), "/bin/sh", "-c", req.Command)
	cmd.Dir = appRoot
	var stdout, stderr cappedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	resp := executeResponse{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			resp.ExitCode = exitErr.ExitCode()
		} else {
			if resp.Stderr != "" {
				resp.Stderr += "\n"
			}
			resp.Stderr += err.Error()
			resp.ExitCode = 1
		}
	}
	writeJSON(w, resp)
}

func upload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseMultipartForm(256 << 20); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer file.Close()
	target, err := safePath(header.Filename)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if err := writeMultipartFile(target, file); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"message": "uploaded"})
}

func download(w http.ResponseWriter, r *http.Request) {
	target, err := safePath(strings.TrimPrefix(r.URL.Path, "/download/"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	http.ServeFile(w, r, target)
}

func list(w http.ResponseWriter, r *http.Request) {
	target, err := safePathAllowRoot(strings.TrimPrefix(r.URL.Path, "/list/"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]listEntry, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		out = append(out, listEntry{Name: entry.Name(), IsDir: entry.IsDir(), Size: info.Size()})
	}
	writeJSON(w, out)
}

func exists(w http.ResponseWriter, r *http.Request) {
	target, err := safePath(strings.TrimPrefix(r.URL.Path, "/exists/"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	_, err = os.Stat(target)
	writeJSON(w, map[string]bool{"exists": err == nil})
}

func safePath(name string) (string, error) {
	return safePathWithRoot(name, false)
}

func safePathAllowRoot(name string) (string, error) {
	return safePathWithRoot(name, true)
}

func safePathWithRoot(name string, allowRoot bool) (string, error) {
	clean := filepath.Clean(strings.TrimLeft(name, "/"))
	if clean == "." {
		if allowRoot {
			return appRoot, nil
		}
		return "", fmt.Errorf("invalid path %q", name)
	}
	if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
		return "", fmt.Errorf("invalid path %q", name)
	}
	target := filepath.Join(appRoot, clean)
	rel, err := filepath.Rel(appRoot, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("path escapes app root")
	}
	return target, nil
}

type cappedBuffer struct {
	bytes.Buffer
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	remaining := maxExecuteOutputBytes - b.Buffer.Len()
	if remaining > 0 {
		if len(p) <= remaining {
			_, _ = b.Buffer.Write(p)
		} else {
			_, _ = b.Buffer.Write(p[:remaining])
			b.truncated = true
		}
	} else if len(p) > 0 {
		b.truncated = true
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string {
	out := b.Buffer.String()
	if b.truncated {
		out += "\n[output truncated]\n"
	}
	return out
}

func writeMultipartFile(target string, file multipart.File) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o700)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, file)
	return err
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
GO

RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} go build -o /out/sandbox-runtime /tmp/sandbox-runtime.go

FROM --platform=$TARGETPLATFORM golang:1.26 AS go-runtime

FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    git \
    make \
    build-essential \
    pkg-config \
    ripgrep \
    gnupg \
 && rm -rf /var/lib/apt/lists/*

# Install Node.js LTS (needed for the published @openai/codex CLI). We use
# NodeSource's official setup script for the matching APT repo + signed key.
ARG NODE_MAJOR=22
RUN set -eux \
 && curl -fsSL "https://deb.nodesource.com/setup_${NODE_MAJOR}.x" | bash - \
 && apt-get install -y --no-install-recommends nodejs \
 && rm -rf /var/lib/apt/lists/*

# Install the Codex CLI globally. The package auto-resolves the correct
# native binary for the target architecture during npm install.
RUN npm install -g @openai/codex && npm cache clean --force

# Install GitHub CLI from the official release tarball. apt's gh package
# can fail on builders behind GPG-mirroring proxies, so we fetch the binary
# directly. Version pinned for reproducibility.
ARG GH_VERSION=2.62.0
RUN set -eux \
 && arch="$(dpkg --print-architecture)" \
 && case "$arch" in \
      amd64) gh_arch=amd64 ;; \
      arm64) gh_arch=arm64 ;; \
      *) echo "unsupported arch: $arch"; exit 1 ;; \
    esac \
 && curl -fsSL -o /tmp/gh.tgz \
      "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_${gh_arch}.tar.gz" \
 && tar -xzf /tmp/gh.tgz -C /tmp \
 && install -m 0755 "/tmp/gh_${GH_VERSION}_linux_${gh_arch}/bin/gh" /usr/local/bin/gh \
 && rm -rf /tmp/gh.tgz "/tmp/gh_${GH_VERSION}_linux_${gh_arch}"

COPY --from=builder /out/worker /worker
COPY --from=builder /out/sandbox-runtime /usr/local/bin/sandbox-runtime
COPY --from=go-runtime /usr/local/go /usr/local/go

RUN ln -sf /usr/local/go/bin/go /usr/local/bin/go \
    && ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt \
    && chmod 0755 /worker /usr/local/bin/sandbox-runtime

ENV GOROOT=/usr/local/go
ENV PATH="/usr/local/go/bin:${PATH}"

RUN groupadd -g 1000 worker \
    && useradd -u 1000 -g worker -m worker \
    && mkdir -p /workspace /app /tmp \
    && chown -R 1000:1000 /workspace /app /home/worker /tmp

USER 1000:1000
ENV HOME=/home/worker
ENTRYPOINT ["/worker"]

# syntax=docker/dockerfile:1
#
# NetworkDoctor node agent image.
#
# Multi-stage build:
#   - build stage: eBPF toolchain (clang/llvm/libbpf/bpftool) + Go.
#     Runs `go generate` (bpf2go compiles networkdoctor.bpf.c and emits the
#     Go bindings that //go:embed the compiled eBPF object), then builds a
#     static Go binary with the eBPF bytecode embedded.
#   - runtime stage: distroless/static, carries only the binary.
#
# The build expects bpf/vmlinux.h to already exist in the build context.
# CI generates it on the Linux runner if it is not committed to the repo.

FROM golang:1.23-bookworm AS build

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        clang \
        llvm \
        libbpf-dev \
        bpftool \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src

# Cache module downloads separately from the source tree.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# bpf/vmlinux.h must be present in the context. As a fallback (e.g. local
# `docker build` on a Linux host), derive it from the host BTF if missing.
RUN if [ ! -f bpf/vmlinux.h ]; then \
        bpftool btf dump file /sys/kernel/btf/vmlinux format c > bpf/vmlinux.h; \
    fi

# Generate eBPF Go bindings (bpf2go) and build a static binary.
RUN go generate ./...
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
        -o /out/networkdoctor-agent ./cmd/networkdoctor-agent

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/networkdoctor-agent /networkdoctor-agent

ENTRYPOINT ["/networkdoctor-agent"]

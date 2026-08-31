# syntax=docker/dockerfile:1
#
# NetworkDoctor node agent image.
#
# Multi-stage build:
#   - build stage: eBPF toolchain (clang/llvm/libbpf) + Go.
#     Runs `go generate` (bpf2go compiles networkdoctor.bpf.c and emits the
#     Go bindings that //go:embed the compiled eBPF object), then builds a
#     static Go binary with the eBPF bytecode embedded.
#   - runtime stage: distroless/static, carries only the binary.
#
# bpf/vmlinux.h is committed to the repo (a pinned CO-RE compile input), so no
# bpftool / BTF dump is needed here.

FROM golang:1.23-bookworm AS build

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        clang \
        llvm \
        libbpf-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src

# Cache module downloads separately from the source tree.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Generate eBPF Go bindings (bpf2go) and build a static binary.
RUN go generate ./...
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
        -o /out/networkdoctor-agent ./cmd/networkdoctor-agent

# Root (uid 0) base on purpose: the agent needs CAP_BPF/CAP_PERFMON/
# CAP_NET_ADMIN (or privileged) to load eBPF. Kubernetes has no ambient
# capability support, so a non-root process cannot make added capabilities
# effective and bpf() fails with EPERM. Do NOT switch to the :nonroot tag.
FROM gcr.io/distroless/static-debian12

COPY --from=build /out/networkdoctor-agent /networkdoctor-agent

ENTRYPOINT ["/networkdoctor-agent"]

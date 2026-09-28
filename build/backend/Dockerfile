# syntax=docker/dockerfile:1
#
# NetworkDoctor backend image.
#
# Pure Go, no eBPF toolchain: the backend only imports internal/backend/*, so
# the linux-only agent packages and bpf2go bindings are never compiled here.

FROM golang:1.23-bookworm AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
        -o /out/networkdoctor-backend ./cmd/networkdoctor-backend

# The backend needs no privileges; run as the distroless non-root user.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/networkdoctor-backend /networkdoctor-backend

USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/networkdoctor-backend"]

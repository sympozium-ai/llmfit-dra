# Stage 1: build llmfit (Rust) from the pinned submodule — the capability
# assessment engine. Hermetic: no host-built binaries, no glibc coupling.
FROM rust:1-slim-bookworm AS llmfit-build
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential pkg-config \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY third_party/llmfit .
RUN cargo build --release -p llmfit

# Stage 2: build the DRA driver (Go).
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY api/go.mod api/go.sum ./api/
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /llmfit-dra ./cmd/llmfit-dra

# Stage 3: runtime. llmfit keys its bandwidth database off lspci device
# names, so pci.ids must be recent enough to know current accelerators
# (bookworm's 2023 database names Strix Halo just "AMD/ATI"). update-pciids
# pulls the latest database at build time.
FROM debian:trixie-slim
# curl stays: the llmfit sidecar's liveness probe execs
# `curl --unix-socket … /health` (kubelet httpGet can't target a UDS).
RUN apt-get update && apt-get install -y --no-install-recommends \
    pciutils curl ca-certificates \
    && update-pciids \
    && rm -rf /var/lib/apt/lists/*
COPY --from=llmfit-build /src/target/release/llmfit /usr/local/bin/llmfit
COPY --from=build /llmfit-dra /llmfit-dra
ENV LLMFIT_BIN=/usr/local/bin/llmfit
ENTRYPOINT ["/llmfit-dra"]

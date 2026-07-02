FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /llmfit-dra ./cmd/llmfit-dra

# Runtime carries the real llmfit binary (Rust, host-built — see `make image`),
# so the base must match its glibc; pciutils+hwdata give llmfit's AMD/NVIDIA
# sysfs paths proper lspci device names for its bandwidth database.
FROM registry.fedoraproject.org/fedora-minimal:44
RUN microdnf install -y pciutils hwdata && microdnf clean all
COPY third_party/llmfit /usr/local/bin/llmfit
COPY --from=build /llmfit-dra /llmfit-dra
ENV LLMFIT_BIN=/usr/local/bin/llmfit
ENTRYPOINT ["/llmfit-dra"]

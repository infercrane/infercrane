# Development-only model server pod for the Modal GPU PoC (tools/poc-modal):
# a reverse proxy to one remote GPU server per pod. It holds no credential and
# no weights; see internal/testtools/modal-gpu-proxy. Never use it in production.
FROM golang:1.26.11-bookworm@sha256:dc40a13b61304647ed7e5d581d246b86413dfc29d63c6d018310feb93cd98fc0 AS builder
WORKDIR /build
COPY go.mod go.sum ./
COPY internal/testtools/modal-gpu-proxy ./internal/testtools/modal-gpu-proxy
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/modal-gpu-proxy ./internal/testtools/modal-gpu-proxy

FROM scratch
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/modal-gpu-proxy /modal-gpu-proxy
USER 65532:65532
ENTRYPOINT ["/modal-gpu-proxy"]

# syntax=docker/dockerfile:1.7
FROM golang:1.26.5-bookworm AS builder
WORKDIR /build
COPY go.mod ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/infercrane ./cmd/infercrane \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/fake-vllm ./internal/testtools/fake-vllm \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/fake-router ./internal/testtools/fake-router

FROM builder AS test
RUN --mount=type=cache,target=/go/pkg/mod go test -race -count=1 ./... \
    && go vet ./...

FROM python:3.12-slim-bookworm AS runtime
ARG VLLM_ROUTER_VERSION=0.1.15
ARG SKYPILOT_VERSION=0.13.0
RUN apt-get update \
    && apt-get install --no-install-recommends -y git openssh-client rsync \
    && rm -rf /var/lib/apt/lists/* \
    && python -m pip install --no-cache-dir "vllm-router==${VLLM_ROUTER_VERSION}" "skypilot[runpod]==${SKYPILOT_VERSION}" \
    && useradd --create-home --uid 10001 app
COPY --from=builder /out/infercrane /usr/local/bin/infercrane
COPY scripts/entrypoint.sh /usr/local/bin/infercrane-entrypoint
RUN chmod 755 /usr/local/bin/infercrane-entrypoint
ENV INFERCRANE_HOST=0.0.0.0
EXPOSE 8080
USER app
ENTRYPOINT ["infercrane-entrypoint"]
CMD ["infercrane", "serve"]

FROM runtime AS development
USER root
COPY --from=builder /out/fake-vllm /usr/local/bin/fake-vllm
COPY --from=builder /out/fake-router /usr/local/bin/fake-router
COPY scripts/dev-start.sh /usr/local/bin/infercrane-dev-start
RUN chmod 755 /usr/local/bin/infercrane-dev-start
USER app

# syntax=docker/dockerfile:1.7
FROM golang:1.26.5-bookworm AS builder
WORKDIR /build
COPY go.mod go.sum ./
# Keep the module cache in the builder layer. The same builder image is used as
# the containerized integration-test runner, so tests must not download the
# dependency graph again every time the container starts.
RUN go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/infercrane ./cmd/infercrane \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/runpod-fault-proxy ./internal/testtools/runpod-fault-proxy \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/fake-vllm ./internal/testtools/fake-vllm \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/fake-router ./internal/testtools/fake-router

FROM builder AS test
RUN --mount=type=cache,target=/go/pkg/mod go test -race -count=1 ./... \
    && go vet ./...

FROM python:3.12-slim-bookworm AS runtime
ARG TARGETARCH
ARG VLLM_ROUTER_VERSION=0.1.15
ARG SKYPILOT_VERSION=0.13.0
ARG HUGGINGFACE_HUB_VERSION=1.24.0
ARG AIPERF_VERSION=0.9.0
ARG AWS_CLI_VERSION=2.27.41
ARG AWS_CLI_AMD64_SHA256=15daae6cc803984064e3d4be9cfd07c4ae8ea703633c0a0b67acc6e321f706a3
ARG AWS_CLI_ARM64_SHA256=2c6ed21cf7cff0a7d77118c69bee867128bf4c588db7b5c044ffba5faeb6ccde
ARG KUBECTL_VERSION=v1.36.0
ARG KUBECTL_AMD64_SHA256=123d8c8844f46b1244c547fffb3c17180c0c26dac9890589fe7e67763298748e
ARG KUBECTL_ARM64_SHA256=9f9d9c44a7b5264515ac9da5991584e2395bd50662e651132337e7b4d0c56f8f
RUN apt-get update \
    && apt-get install --no-install-recommends -y build-essential ca-certificates curl git openssh-client rsync unzip \
	&& python -m pip install --no-cache-dir "vllm-router==${VLLM_ROUTER_VERSION}" "skypilot[runpod]==${SKYPILOT_VERSION}" "aiperf==${AIPERF_VERSION}" \
	&& python -m venv /opt/infercrane-huggingface \
	&& /opt/infercrane-huggingface/bin/pip install --no-cache-dir "huggingface_hub[hf_xet]==${HUGGINGFACE_HUB_VERSION}" \
	&& case "$TARGETARCH" in \
	     amd64) aws_arch=x86_64; aws_sha="$AWS_CLI_AMD64_SHA256"; kubectl_sha="$KUBECTL_AMD64_SHA256" ;; \
	     arm64) aws_arch=aarch64; aws_sha="$AWS_CLI_ARM64_SHA256"; kubectl_sha="$KUBECTL_ARM64_SHA256" ;; \
	     *) echo "unsupported target architecture: $TARGETARCH" >&2; exit 1 ;; \
	   esac \
	&& curl -fsSLo /tmp/awscliv2.zip "https://awscli.amazonaws.com/awscli-exe-linux-${aws_arch}-${AWS_CLI_VERSION}.zip" \
	&& printf '%s  %s\n' "$aws_sha" /tmp/awscliv2.zip | sha256sum -c - \
	&& unzip -q /tmp/awscliv2.zip -d /tmp \
	&& /tmp/aws/install --install-dir /usr/local/aws-cli --bin-dir /usr/local/bin \
	&& curl -fsSLo /usr/local/bin/kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl" \
	&& printf '%s  %s\n' "$kubectl_sha" /usr/local/bin/kubectl | sha256sum -c - \
	&& chmod 0755 /usr/local/bin/kubectl \
	&& aws --version \
	&& kubectl version --client=true \
	&& rm -rf /tmp/aws /tmp/awscliv2.zip \
	&& apt-get purge -y build-essential curl unzip \
	&& apt-get autoremove -y \
	&& rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 10001 app
COPY --from=builder /out/infercrane /usr/local/bin/infercrane
COPY scripts/entrypoint.sh /usr/local/bin/infercrane-entrypoint
RUN chmod 755 /usr/local/bin/infercrane-entrypoint
ENV INFERCRANE_HOST=0.0.0.0
ENV INFERCRANE_HF_PYTHON=/opt/infercrane-huggingface/bin/python
EXPOSE 8080
USER app
ENTRYPOINT ["infercrane-entrypoint"]
CMD ["infercrane", "serve"]

FROM runtime AS acceptance
USER root
COPY --from=builder /out/runpod-fault-proxy /usr/local/bin/runpod-fault-proxy
USER app

FROM runtime AS development
USER root
COPY --from=builder /out/fake-vllm /usr/local/bin/fake-vllm
COPY --from=builder /out/fake-router /usr/local/bin/fake-router
COPY scripts/dev-start.sh /usr/local/bin/infercrane-dev-start
RUN chmod 755 /usr/local/bin/infercrane-dev-start
USER app

# syntax=docker/dockerfile:1.7
FROM golang:1.26.11-bookworm@sha256:dc40a13b61304647ed7e5d581d246b86413dfc29d63c6d018310feb93cd98fc0 AS builder
ARG INFERCRANE_VERSION=2.0.1
WORKDIR /build
COPY go.mod go.sum ./
# Keep the module cache in the builder layer. The same builder image is used as
# the containerized integration-test runner, so tests must not download the
# dependency graph again every time the container starts.
RUN go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod version="${INFERCRANE_VERSION#v}" \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${version}" -o /out/infercrane ./cmd/infercrane \
	&& CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/infercrane-openrouter-edge ./cmd/infercrane-openrouter-edge \
	&& CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/infercrane-model-api-qualifier ./tools/model-api-qualifier \
	&& CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/infercrane-model-api-production-release ./tools/model-api-production-release \
	&& CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/infercrane-model-api-mvp-release ./tools/model-api-mvp-release \
	&& CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/infercrane-model-api-renewer ./tools/model-api-renewer \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/runpod-fault-proxy ./internal/testtools/runpod-fault-proxy \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/fake-vllm ./internal/testtools/fake-vllm \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/fake-router ./internal/testtools/fake-router \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/capture-proxy ./internal/testtools/capture-proxy
RUN --mount=type=cache,target=/go/pkg/mod ./scripts/generate-go-license-bundle.sh /out/licenses/go

FROM scratch AS openrouter-edge
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/infercrane-openrouter-edge /infercrane-openrouter-edge
COPY deploy/openrouter/qwen38-provider-models.json /etc/infercrane/provider-models.json
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/infercrane-openrouter-edge"]

FROM builder AS test
RUN --mount=type=cache,target=/go/pkg/mod go test -race -count=1 ./... \
    && go vet ./...

FROM python:3.12-slim-trixie@sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b AS runtime
ARG TARGETARCH
ARG VLLM_ROUTER_VERSION=0.1.15
ARG SKYPILOT_VERSION=0.13.0
# SkyPilot clouds the image can drive. Each still needs a provider manifest and its credentials.
# SkyPilot 0.13.0 passes extra keyword arguments to FastAPI(); FastAPI 0.145 removed them, so
# `sky api start` exits at boot. Keep fastapi below 0.145 until SkyPilot is upgraded.
# SkyPilot 0.13.0 reads the Vast key through VastAI().client, which vastai-sdk added in 1.0;
# without the floor pip may resolve 0.2.x and every Vast launch fails.
ARG SKYPILOT_EXTRAS=runpod,vast
ARG HUGGINGFACE_HUB_VERSION=1.24.0
ARG AIPERF_VERSION=0.12.0
ARG AWS_CLI_VERSION=2.42.1
ARG AWS_CLI_AMD64_SHA256=ff2631c32c5db291b099600f404bc493454ec66c125ff4604625da90480c1f47
ARG AWS_CLI_ARM64_SHA256=98f5f03bb6508a06ce9d9d6d2db97932dce735f3b7ad85616324a95edf4b815f
ARG GCLOUD_CLI_VERSION=577.0.0
ARG GCLOUD_CLI_AMD64_SHA256=0b32d330446ce7b0f57f253e7efab4636c18fb1f87a3ac31c6c3f2a2a697525e
ARG GCLOUD_CLI_ARM64_SHA256=dbac26bdf80d72b5d13538e3a215dcbfe2781edfd2d69723effbeef3839cffb8
ARG KUBECTL_VERSION=v1.37.2
ARG KUBECTL_AMD64_SHA256=c04487523e7aa7d1493f7808174c899db93d7a9cf57531420cd76b9bd415f8f4
ARG KUBECTL_ARM64_SHA256=fcc16f63186659d9722014b7e02d5fdd2d4e669cb8c0ab66365d7d3b8ba03509
# Two pinned tools hold vulnerable dependencies; replace them in place after the resolver runs.
# AIPerf 0.12.0 pins Pillow 12.2 (critical advisories fixed in 13.0). AIPerf uses Pillow only to
# synthesize image prompts, which InferCrane benchmarks never send. vastai 1.0.0 caps cryptography at
# 46.0.4 (CVE-2026-39892, fixed in the 46.0.7 patch release). Drop each override when the AIPerf pin
# moves to 0.14 or vastai-sdk resolves a newer vastai.
ARG PILLOW_VERSION=13.0.0
ARG CRYPTOGRAPHY_VERSION=46.0.7
RUN apt-get update \
    && apt-get upgrade -y \
    && apt-get install --no-install-recommends -y build-essential ca-certificates curl git openssh-client rsync unzip \
	&& python -m pip install --no-cache-dir --upgrade pip \
	&& python -m pip install --no-cache-dir "vllm-router==${VLLM_ROUTER_VERSION}" "skypilot[${SKYPILOT_EXTRAS}]==${SKYPILOT_VERSION}" "fastapi<0.145" "vastai-sdk>=1.0,<2" "aiperf==${AIPERF_VERSION}" \
	&& python -m pip install --no-cache-dir --no-deps "pillow==${PILLOW_VERSION}" "cryptography==${CRYPTOGRAPHY_VERSION}" \
	&& python -c "import sky, cryptography, PIL" \
	&& aiperf --version \
	&& python -m venv /opt/infercrane-huggingface \
	&& /opt/infercrane-huggingface/bin/pip install --no-cache-dir "huggingface_hub[hf_xet]==${HUGGINGFACE_HUB_VERSION}"
RUN case "$TARGETARCH" in \
	     amd64) aws_arch=x86_64; aws_sha="$AWS_CLI_AMD64_SHA256"; gcloud_arch=x86_64; gcloud_sha="$GCLOUD_CLI_AMD64_SHA256"; kubectl_sha="$KUBECTL_AMD64_SHA256" ;; \
	     arm64) aws_arch=aarch64; aws_sha="$AWS_CLI_ARM64_SHA256"; gcloud_arch=arm; gcloud_sha="$GCLOUD_CLI_ARM64_SHA256"; kubectl_sha="$KUBECTL_ARM64_SHA256" ;; \
	     *) echo "unsupported target architecture: $TARGETARCH" >&2; exit 1 ;; \
	   esac \
	&& curl -fsSLo /tmp/awscliv2.zip "https://awscli.amazonaws.com/awscli-exe-linux-${aws_arch}-${AWS_CLI_VERSION}.zip" \
	&& printf '%s  %s\n' "$aws_sha" /tmp/awscliv2.zip | sha256sum -c - \
	&& unzip -q /tmp/awscliv2.zip -d /tmp \
	&& /tmp/aws/install --install-dir /usr/local/aws-cli --bin-dir /usr/local/bin \
	&& curl -fsSLo /tmp/google-cloud-cli.tar.gz "https://storage.googleapis.com/cloud-sdk-release/google-cloud-cli-${GCLOUD_CLI_VERSION}-linux-${gcloud_arch}.tar.gz" \
	&& printf '%s  %s\n' "$gcloud_sha" /tmp/google-cloud-cli.tar.gz | sha256sum -c - \
	&& tar -xzf /tmp/google-cloud-cli.tar.gz -C /usr/local \
	&& ln -s /usr/local/google-cloud-sdk/bin/gcloud /usr/local/bin/gcloud \
	&& curl -fsSLo /usr/local/bin/kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl" \
	&& printf '%s  %s\n' "$kubectl_sha" /usr/local/bin/kubectl | sha256sum -c - \
	&& chmod 0755 /usr/local/bin/kubectl \
	&& aws --version \
	&& gcloud --version \
	&& kubectl version --client=true \
	&& rm -rf /tmp/aws /tmp/awscliv2.zip /tmp/google-cloud-cli.tar.gz \
	&& apt-get purge -y build-essential curl unzip \
	&& apt-get autoremove -y \
	&& rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 10001 app
COPY --from=builder /out/infercrane /usr/local/bin/infercrane
COPY --from=builder /out/infercrane-model-api-qualifier /usr/local/bin/infercrane-model-api-qualifier
COPY --from=builder /out/infercrane-model-api-production-release /usr/local/bin/infercrane-model-api-production-release
COPY --from=builder /out/infercrane-model-api-mvp-release /usr/local/bin/infercrane-model-api-mvp-release
COPY --from=builder /out/infercrane-model-api-renewer /usr/local/bin/infercrane-model-api-renewer
COPY scripts/entrypoint.sh /usr/local/bin/infercrane-entrypoint
COPY deploy/openrouter/qwen38-provider-models.json /etc/infercrane/openrouter-provider-models.json
COPY LICENSE NOTICE THIRD_PARTY_NOTICES.md packaging/container/THIRD_PARTY_COMPONENTS.md /usr/share/licenses/infercrane/
COPY packaging/container/licenses /usr/share/licenses/infercrane/runtime-components
COPY --from=builder /out/licenses/go /usr/share/licenses/infercrane/go
RUN chmod 755 /usr/local/bin/infercrane-entrypoint
LABEL org.opencontainers.image.licenses="Apache-2.0"
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
COPY --from=builder /out/capture-proxy /usr/local/bin/capture-proxy
COPY scripts/dev-start.sh /usr/local/bin/infercrane-dev-start
RUN chmod 755 /usr/local/bin/infercrane-dev-start
USER app

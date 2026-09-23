ARG GO_BUILDER_IMAGE=golang:1.26.6-bookworm
ARG SGLANG_IMAGE=lmsysorg/sglang@sha256:06e4f2ed21afde4ff513cda65070124e727ba23ccaeff7712b8c40e1097d611f
FROM ${GO_BUILDER_IMAGE} AS edge-builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/infercrane-openrouter-edge ./cmd/infercrane-openrouter-edge
COPY internal/openrouterprovider ./internal/openrouterprovider
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -trimpath -ldflags='-s -w -buildid=' \
    -o /out/infercrane-openrouter-edge ./cmd/infercrane-openrouter-edge

FROM ${SGLANG_IMAGE}

LABEL org.opencontainers.image.title="InferCrane Qwen3.8 OpenRouter candidate" \
      org.opencontainers.image.description="Pinned SGLang 0.5.20 recipe with an admission-controlled OpenRouter provider edge" \
      ai.infercrane.model.repository="Qwen/Qwen3.8-27B-FP8" \
      ai.infercrane.model.revision="017b9c7af6b5689d5dd426a76e0bc077eb5ca20a" \
      ai.infercrane.runtime="sglang-0.5.20" \
      ai.infercrane.recipe="nextn-k4-bounded-graphs" \
      ai.infercrane.status="target-provider-qualification-required"

COPY --from=edge-builder /out/infercrane-openrouter-edge /usr/local/bin/infercrane-openrouter-edge
COPY deploy/openrouter/qwen38-provider-models.json /opt/infercrane/provider-models.json
COPY deploy/openrouter/qwen38-sglang-0520/health_shim.py /opt/infercrane/health_shim.py
COPY deploy/openrouter/qwen38-sglang-0520/prepare_model.py /opt/infercrane/prepare_model.py
COPY deploy/openrouter/qwen38-sglang-0520/serve.sh /opt/infercrane/serve.sh

RUN chmod 0555 /usr/local/bin/infercrane-openrouter-edge /opt/infercrane/serve.sh \
    && chmod 0444 /opt/infercrane/provider-models.json /opt/infercrane/health_shim.py /opt/infercrane/prepare_model.py \
    && mkdir -p /runpod-volume /run/infercrane \
    && test ! -e /model-cache \
    && ln -s /runpod-volume /model-cache

ENV HF_HOME=/model-cache \
    HF_HUB_CACHE=/model-cache/hub \
    TRANSFORMERS_OFFLINE=1 \
    HF_HUB_OFFLINE=1 \
    INFERCRANE_OPENROUTER_LISTEN=:8080 \
    INFERCRANE_OPENROUTER_PROVIDER_CATALOG_FILE=/opt/infercrane/provider-models.json \
    INFERCRANE_OPENROUTER_PUBLIC_MODEL=qwen/qwen3.8-27b \
    INFERCRANE_OPENROUTER_UPSTREAM_MODEL=Qwen/Qwen3.8-27B-FP8 \
    INFERCRANE_OPENROUTER_UPSTREAM_URL=http://127.0.0.1:30000 \
    PORT=8080 \
    PORT_HEALTH=30001 \
    HEALTH_CHECK_PATH=/ping

EXPOSE 8080 30001

HEALTHCHECK --interval=30s --timeout=5s --start-period=20m --retries=3 \
  CMD /usr/bin/python3 -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8080/readyz', timeout=3)"

ENTRYPOINT ["/opt/infercrane/serve.sh"]
CMD []

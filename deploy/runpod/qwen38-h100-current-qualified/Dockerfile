ARG SGLANG_BASE_IMAGE=lmsysorg/sglang@sha256:9e148f5ac788e856a06166bd6347a831831eb9fcfab4d1770874823a7c29a1a1
FROM ${SGLANG_BASE_IMAGE}

LABEL org.opencontainers.image.title="InferCrane provisional Qwen3.8 RunPod candidate" \
      org.opencontainers.image.description="Modal-qualified candidate; requires independent RunPod qualification" \
      ai.infercrane.package="qwen38-h100-current-qualified" \
      ai.infercrane.status="provisional_faststart_candidate" \
      ai.infercrane.runtime="sglang-0.5.18" \
      ai.infercrane.model.repository="Qwen/Qwen3.8-27B-FP8" \
      ai.infercrane.model.revision="017b9c7af6b5689d5dd426a76e0bc077eb5ca20a" \
      ai.infercrane.qualification.provider="modal" \
      ai.infercrane.qualification.runpod="required"

COPY modal-overlay-requirements.txt /opt/infercrane/modal-overlay-requirements.txt
RUN /usr/bin/python3 -m pip install --no-cache-dir \
    -r /opt/infercrane/modal-overlay-requirements.txt

COPY health_shim.py /opt/infercrane/health_shim.py
COPY manifest.json /opt/infercrane/deployment-manifest.json
COPY serve.sh /opt/infercrane/serve.sh
RUN chmod 0444 \
      /opt/infercrane/health_shim.py \
      /opt/infercrane/deployment-manifest.json \
      /opt/infercrane/modal-overlay-requirements.txt \
    && chmod 0555 /opt/infercrane/serve.sh \
    && test ! -e /model-cache \
    && mkdir -p /runpod-volume \
    && ln -s /runpod-volume /model-cache

# RunPod Serverless network volumes are mounted at /runpod-volume. The stable
# /model-cache symlink preserves the path used by the Modal-qualified recipe.
ENV HF_HOME=/model-cache \
    HF_HUB_CACHE=/model-cache/hub \
    HF_HUB_OFFLINE=1 \
    TRANSFORMERS_OFFLINE=1 \
    PORT=30000 \
    PORT_HEALTH=30001 \
    HEALTH_CHECK_PATH=/ping

EXPOSE 30000 30001

# Docker health remains strict readiness. RunPod must probe /ping on 30001,
# where the shim maps SGLang warmup to RunPod's HTTP 204 initialization state.
HEALTHCHECK --interval=30s --timeout=5s --start-period=20m --retries=3 \
  CMD /usr/bin/python3 -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:30000/health', timeout=3)"

ENTRYPOINT ["/opt/infercrane/serve.sh"]
CMD []

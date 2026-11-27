ARG VLLM_IMAGE=vllm/vllm-openai@sha256:cf5b2fdd087eb4f6d261736967cc5cde974bd22ff4df50365f8fa419b6b4c484
FROM ${VLLM_IMAGE}

# vLLM 0.34.0 with InferCrane's state-by-draft patch (infercrane-state-by-draft-v4), the engine of the
# technique adaptive-speculation-one-engine. Each request reserves the recurrent state of the draft
# length vLLM's per-batch schedule gives the batch it joins, not of the longest. The patch refuses any
# vLLM source whose sha256 it does not know, so a changed base fails this build. The receipt it writes
# is what the product checks before it gives this image the technique's flags.
LABEL org.opencontainers.image.title="InferCrane vLLM 0.34.0, recurrent state sized by draft length" \
      org.opencontainers.image.description="vLLM 0.34.0 with infercrane-state-by-draft-v4 applied at build; serves adaptive-speculation-one-engine only" \
      ai.infercrane.runtime="vllm-0.34.0" \
      ai.infercrane.patch="infercrane-state-by-draft-v4" \
      ai.infercrane.patch.receipt="/opt/infercrane/state-by-draft-receipt.json"

COPY deploy/vllm-state-by-draft/patch_state_by_draft.py /opt/infercrane/patch_state_by_draft.py
COPY deploy/vllm-state-by-draft/check_state_by_draft.py /opt/infercrane/check_state_by_draft.py
COPY deploy/vllm-state-by-draft/verify_state_by_draft.py /opt/infercrane/verify_state_by_draft.py
COPY deploy/vllm-state-by-draft/serve-state-by-draft /opt/infercrane/serve-state-by-draft

RUN chmod 0444 /opt/infercrane/patch_state_by_draft.py /opt/infercrane/check_state_by_draft.py /opt/infercrane/verify_state_by_draft.py \
    && python3 /opt/infercrane/patch_state_by_draft.py --receipt /opt/infercrane/state-by-draft-receipt.json \
    && python3 /opt/infercrane/check_state_by_draft.py \
    && python3 /opt/infercrane/verify_state_by_draft.py /opt/infercrane/state-by-draft-receipt.json \
    && python3 -c "import sys, vllm; print(sys.executable)" > /opt/infercrane/engine-python \
    && chmod 0444 /opt/infercrane/state-by-draft-receipt.json /opt/infercrane/engine-python \
    && chmod 0555 /opt/infercrane/serve-state-by-draft

# The technique's setups start through the receipt check; so does a plain `docker run`.
ENTRYPOINT ["/opt/infercrane/serve-state-by-draft"]

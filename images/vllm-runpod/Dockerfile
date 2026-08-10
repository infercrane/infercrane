# syntax=docker/dockerfile:1.7

# RunPod's base starts /start.sh, including the SSH daemon SkyPilot needs to
# finish configuring a Pod. Pin the base by digest and install the tested vLLM
# stack at image-build time so replicas never pip-install CUDA dependencies.
FROM runpod/pytorch:2.4.0-py3.11-cuda12.4.1-devel-ubuntu22.04@sha256:61a4aafb0094cd773f11eefa378929d5a687bd775febeb78eac62fc824141fb5

ARG VLLM_VERSION=0.8.5.post1
ARG TRANSFORMERS_VERSION=4.51.3

RUN python -m pip install --no-cache-dir \
      "vllm==${VLLM_VERSION}" \
      "transformers==${TRANSFORMERS_VERSION}" \
    && python -m pip check \
    && vllm --version

# Preserve the base image ENTRYPOINT/CMD. SkyPilot connects over the RunPod
# managed TCP mapping and starts the inference command as a durable task.

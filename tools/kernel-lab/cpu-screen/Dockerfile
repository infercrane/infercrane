# CPU-only image for checking generated Triton kernels on a laptop with
# Triton's interpreter. It proves correctness, never performance.
FROM python:3.12-slim@sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b
RUN pip install --no-cache-dir --index-url https://download.pytorch.org/whl/cpu torch==2.15.0 \
 && pip install --no-cache-dir triton==3.9.0 numpy==2.5.4 \
 && python -c "import torch, triton, numpy"
ENV TRITON_INTERPRET=1 INFERCRANE_EVAL_DEVICE=cpu
USER 65534:65534

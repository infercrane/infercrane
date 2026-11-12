# Development-only model server image for the local PoC: the pinned llama.cpp
# server with one pinned GGUF baked in, so each candidate pod starts without
# downloading the weights again. The GGUF is fetched and checked by
# tools/poc-local/poc.py before the build.
ARG BASE
FROM ${BASE}
COPY model.gguf /models/model.gguf
ENTRYPOINT ["/app/llama-server"]

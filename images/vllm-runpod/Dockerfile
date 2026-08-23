# syntax=docker/dockerfile:1.7

# Keep the tested runtime byte-for-byte and add only RunPod's SSH bootstrap
# contract. SkyPilot connects over the managed TCP mapping and starts vLLM as
# its durable task; no Python or CUDA dependency is installed on the Pod.
ARG VLLM_BASE_IMAGE="vllm/vllm-openai@sha256:0fec7ec5f3e6bc168e54899935fb0557da908a4832a1dbc88e2debcf2f889416"
FROM ${VLLM_BASE_IMAGE}
ARG VLLM_VERSION="0.22.0"

# Never allow the publication workflow to attach a requested version tag to
# different runtime bytes. A patched candidate supplies both an immutable base
# digest and the version expected inside that image.
RUN python -c 'import importlib.metadata, sys; actual = importlib.metadata.version("vllm"); expected = sys.argv[1]; assert actual == expected, f"vLLM version mismatch: expected {expected}, found {actual}"' "$VLLM_VERSION"

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install --yes --no-install-recommends openssh-server rsync \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /run/sshd /root/.ssh \
    && chmod 700 /root/.ssh

COPY images/vllm-runpod/start-ssh.sh /usr/local/bin/infercrane-start-ssh
RUN chmod 755 /usr/local/bin/infercrane-start-ssh

EXPOSE 22
ENTRYPOINT ["/usr/local/bin/infercrane-start-ssh"]

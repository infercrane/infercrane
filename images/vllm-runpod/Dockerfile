# syntax=docker/dockerfile:1.7

# Keep the tested runtime byte-for-byte and add only RunPod's SSH bootstrap
# contract. SkyPilot connects over the managed TCP mapping and starts vLLM as
# its durable task; no Python or CUDA dependency is installed on the Pod.
FROM vllm/vllm-openai@sha256:c48cf118e1e6e39d7790e174d6014f7af5d06f79c2d29d984d11cbe2e8d414e7

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install --yes --no-install-recommends openssh-server rsync \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /run/sshd /root/.ssh \
    && chmod 700 /root/.ssh

COPY images/vllm-runpod/start-ssh.sh /usr/local/bin/infercrane-start-ssh
RUN chmod 755 /usr/local/bin/infercrane-start-ssh

EXPOSE 22
ENTRYPOINT ["/usr/local/bin/infercrane-start-ssh"]

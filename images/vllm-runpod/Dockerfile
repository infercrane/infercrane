# syntax=docker/dockerfile:1.7

# Keep the tested runtime byte-for-byte and add only RunPod's SSH bootstrap
# contract. SkyPilot connects over the managed TCP mapping and starts vLLM as
# its durable task; no Python or CUDA dependency is installed on the Pod.
ARG VLLM_BASE_IMAGE="vllm/vllm-openai@sha256:953d3a06d5e64ab582985cd7401289d3abf2a2c14ef2158e9a84313daeec77d7"
FROM ${VLLM_BASE_IMAGE}
ARG VLLM_VERSION="0.22.1"

# Never allow the publication workflow to attach a requested version tag to
# different runtime bytes. A patched candidate supplies both an immutable base
# digest and the version expected inside that image.
RUN python3 -c 'import importlib.metadata, sys; actual = importlib.metadata.version("vllm"); expected = sys.argv[1]; assert actual == expected, f"vLLM version mismatch: expected {expected}, found {actual}"' "$VLLM_VERSION"

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install --yes --no-install-recommends openssh-server rsync \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /run/sshd /root/.ssh \
    && chmod 700 /root/.ssh

# The upstream Python dependency graph contains redundant statically linked
# ffmpeg executables in both installed packages and package-manager caches.
# Force every media consumer through Ubuntu's maintained system package and
# remove every bundled copy from the final root filesystem.
RUN find / -xdev -type f -name 'ffmpeg*' ! -path /usr/bin/ffmpeg -delete \
    && test -x /usr/bin/ffmpeg \
    && /usr/bin/ffmpeg -version | grep -q '^ffmpeg version '
ENV IMAGEIO_FFMPEG_EXE=/usr/bin/ffmpeg

COPY images/vllm-runpod/start-ssh.sh /usr/local/bin/infercrane-start-ssh
COPY LICENSE NOTICE images/vllm-runpod/THIRD_PARTY_COMPONENTS.md /usr/share/licenses/infercrane/
RUN chmod 755 /usr/local/bin/infercrane-start-ssh
LABEL org.opencontainers.image.licenses="Apache-2.0"

EXPOSE 22
ENTRYPOINT ["/usr/local/bin/infercrane-start-ssh"]

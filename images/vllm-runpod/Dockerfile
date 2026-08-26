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

# vLLM permits OpenCV >=4.13, so the upstream image can resolve to 5.0.0.93,
# whose wheel bundles vulnerable FFmpeg 8.0.1 libraries. OpenCV 4.14.0.94 is
# requirement-compatible and was built after the 4.x FFmpeg 8.1.2 security
# bump. Install its exact amd64 wheel by hash so the derived image is
# reproducible and retains OpenCV video support.
RUN python3 -m pip install --no-cache-dir --no-deps --force-reinstall \
      'opencv-python-headless @ https://files.pythonhosted.org/packages/ae/ce/7f538891722a4f06921419d55bac6f41729258d54442861edb2de0dbdf5e/opencv_python_headless-4.14.0.94-cp37-abi3-manylinux_2_28_x86_64.whl#sha256=211e581f5a4670acbbe08fff36a35e9946039d2eea28b80394632d036d1be527' \
    && python3 -c 'import cv2, importlib.metadata; assert importlib.metadata.version("opencv-python-headless") == "4.14.0.94"; assert cv2.__version__ == "4.14.0"'

# The upstream Python dependency graph also contains redundant standalone
# ffmpeg executables in installed packages and package-manager caches. Force
# those consumers through Ubuntu's maintained system package and remove every
# standalone bundled copy from the final root filesystem.
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

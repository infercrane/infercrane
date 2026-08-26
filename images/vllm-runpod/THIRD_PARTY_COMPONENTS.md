# vLLM RunPod image license inventory

This image adds InferCrane's Apache-2.0-licensed SSH bootstrap to the immutable vLLM base image and
installs Debian's `openssh-server` and `rsync` packages. InferCrane's `LICENSE` and `NOTICE` are in
this directory. Redundant bundled ffmpeg executables are removed and media consumers are directed to
the base image's Ubuntu-maintained `/usr/bin/ffmpeg`. The base image and Debian packages retain their component-native license and notice
files; the publication workflow also produces an SPDX SBOM for the exact image digest.

The SBOM and component-native files are authoritative for the built image. Model weights and any
runtime-downloaded artifacts are separate works governed by their own terms.

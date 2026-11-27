package support

import (
	"fmt"
	"slices"

	"github.com/infercrane/infercrane/internal/provideridentity"
)

// ImageNotQualified marks an engine build recorded for an accelerator
// platform that is not qualified (provideridentity.AcceleratorNotQualified).
const ImageNotQualified = "not-qualified"

// notQualifiedImages are the engine builds a first qualification run on a
// platform that is not qualified would pin. They live apart from
// runtimeImages, so no selection reads them: PinnedImage, RuntimeVersions,
// RuntimePlatforms, RuntimeWorkload, RuntimeImages, CurrentRuntimeImages, and
// the integration catalog see only runtimeImages, and a spec on these
// platforms is refused. Qualifying a platform moves its build into
// runtimeImages, its accelerators to AcceleratorServing, and its placement
// into V1, together with the measured run that justifies it. Each digest was
// read from the registry's manifest for Tag; a tag can move, the digest
// cannot.
var notQualifiedImages = slices.Concat(tpuCandidateImages, intelCandidateImages)

// tpuCandidateImages: the TPU plugins' own release builds, both amd64 only.
// vLLM JAX (formerly tpu-inference) serves JAX model implementations and,
// through torchax, vLLM's PyTorch ones; vLLM TorchTPU serves PyTorch model
// definitions through torch_tpu. Both run "vllm serve". A first run picks one
// per model; both are recorded so neither is guessed. Digests read from
// registry-1.docker.io on 2026-11-26.
var tpuCandidateImages = func() []RuntimeImage {
	builds := []RuntimeImage{
		{Runtime: DefaultRuntime, EngineVersion: "0.34.0", Status: ImageNotQualified,
			Image: "vllm/vllm-tpu@sha256:450e37c04b8bf4dd2568e38672dcd94d0f22f131510962725d577eb249839440", Tag: "vllm/vllm-tpu:v0.34.0",
			Stack: "vLLM JAX (tpu-inference) 0.34.0, JAX 0.11.2, torchax 0.0.13, libtpu 0.0.48",
			Note:  "Built by tpu-inference v0.34.0 (tag commit 6c818712b) against vLLM main commit 5ec99956ad, not vLLM's v0.34.0 tag, so the engine defaults InferCrane records for vLLM 0.34.0 do not carry over until read from this build."},
		{Runtime: DefaultRuntime, EngineVersion: "0.33.0", Status: ImageNotQualified,
			Image: "vllm/vllm-torchtpu@sha256:95e38744cf05e337a47901518a70fc3308639a11f0c9a87066f0840ebb00f020", Tag: "vllm/vllm-torchtpu:v0.33.0",
			Stack: "vLLM TorchTPU 0.33.0, vLLM 0.33.0, torch 2.13.0, torch_tpu 0.2.0, libtpu 0.0.50",
			Note:  "Built by vllm-torchtpu v0.33.0 (tag commit 46ae4b7a6) on vLLM v0.33.0, one release behind the vLLM InferCrane pins for GPUs."},
	}
	var out []RuntimeImage
	for _, platform := range []struct{ id, target string }{
		{provideridentity.PlatformTPUV5E, "v5e"}, {provideridentity.PlatformTPUV5P, "v5p"},
		{provideridentity.PlatformTPUV6E, "v6e"}, {provideridentity.PlatformTPUV7X, "v7x"},
	} {
		for _, build := range builds {
			build.Platform, build.Targets = platform.id, []string{platform.target}
			out = append(out, build)
		}
	}
	return out
}()

// NotQualifiedRuntimeImages returns the engine builds recorded for platforms
// that are not qualified. Nothing selects them.
func NotQualifiedRuntimeImages() []RuntimeImage {
	out := make([]RuntimeImage, len(notQualifiedImages))
	for index, image := range notQualifiedImages {
		image.Targets = slices.Clone(image.Targets)
		out[index] = image
	}
	return out
}

// RefuseNotQualified refuses an accelerator InferCrane recognizes but has not
// qualified, whatever the matrix or runtime: nothing may select it until a
// measured run qualifies its platform.
func RefuseNotQualified(gpu string) error {
	accelerator, ok := provideridentity.IdentifyAccelerator(gpu)
	if !ok || accelerator.Status == provideridentity.AcceleratorServing {
		return nil
	}
	return fmt.Errorf("%s (%s) is on the %s platform, which is not qualified: no runtime image or placement may select it until a measured run qualifies the platform", gpu, accelerator.Product, accelerator.Platform)
}

// intelCandidateImages: the vLLM Gaudi plugin's build from Intel's registry
// and vLLM's own XPU build. Digests read on 2026-11-26 from vault.habana.ai
// (anonymous token) and registry-1.docker.io.
var intelCandidateImages = func() []RuntimeImage {
	gaudi := RuntimeImage{Runtime: DefaultRuntime, EngineVersion: "0.30.0", Status: ImageNotQualified,
		Image: "vault.habana.ai/gaudi-docker/1.24.2/ubuntu24.04/habanalabs/vllm-0.30.0-ptupstream-2.13.0@sha256:5d064465d751a73a729e7ecdc32c0ba5de0df1d305debb60a3d9a85b4fd3a967",
		Tag:   "vault.habana.ai/gaudi-docker/1.24.2/ubuntu24.04/habanalabs/vllm-0.30.0-ptupstream-2.13.0:1.24.2-481",
		Stack: "vLLM Gaudi plugin 0.30.0 on vLLM 0.30.0, Intel Gaudi software 1.24.2, PyTorch 2.13.0",
		Note:  "Intel's build, amd64 only, four vLLM releases behind the GPU images. The Intel Gaudi software inside is proprietary (Intel Gaudi End User Software License Agreement): run by digest, never re-hosted. Its entrypoint is Intel's launcher, so the workload must set the vllm serve command."}
	xpu := RuntimeImage{Runtime: DefaultRuntime, EngineVersion: "0.34.0", Status: ImageNotQualified,
		Image: "vllm/vllm-openai-xpu@sha256:cb55d8cffeb1851c552c4b002a5e8be21daa99a51a678d53c57f271e7c16b3f0", Tag: "vllm/vllm-openai-xpu:v0.34.0",
		Stack: "vLLM 0.34.0, PyTorch 2.15.0 (XPU), vllm_xpu_kernels 0.1.17, Intel OMIX 0.4.1",
		Note:  "vLLM's own release build for Intel GPUs, amd64 only. vLLM 0.34.0 validates only Arc Pro B-Series GPUs on it. The oneAPI runtime libraries inside are under Intel's licences, not Apache-2.0."}
	var out []RuntimeImage
	for _, platform := range []struct {
		id, target string
		image      RuntimeImage
	}{
		{provideridentity.PlatformHPUGaudi2, "gaudi2", gaudi},
		{provideridentity.PlatformHPUGaudi3, "gaudi3", gaudi},
		{provideridentity.PlatformXPUXe2, "bmg", xpu},
	} {
		image := platform.image
		image.Platform, image.Targets = platform.id, []string{platform.target}
		out = append(out, image)
	}
	return out
}()

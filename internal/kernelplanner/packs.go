package kernelplanner

import (
	"slices"
	"strings"
)

// ReviewedPack is a kernel operator pack the Accelerator Lab can write,
// build, and qualify (tools/kernel-lab/packs). Its serving integration was
// reviewed against exact engine versions only; an engine change means
// qualifying again or turning the kernel off. Operator is the GPU trace
// analysis' operator class (gpuprofile.Operators) the pack replaces.
type ReviewedPack struct {
	Family   OperatorFamily `json:"family"`
	Operator string         `json:"operator"`
	Vendor   string         `json:"vendor"`
	Engine   string         `json:"engine"`
	Versions []string       `json:"versions"`
}

// reviewedPacks mirrors each pack.json; a test keeps them in step.
var reviewedPacks = []ReviewedPack{
	{Family: ResidualRMSNorm, Operator: "norm", Vendor: "nvidia", Engine: "vllm", Versions: []string{"0.22.1"}},
	{Family: MoEExperts, Operator: "moe-experts", Vendor: "nvidia", Engine: "vllm", Versions: []string{"0.22.1"}},
}

// ReviewedPacks lists the packs the lab can qualify today.
func ReviewedPacks() []ReviewedPack {
	out := make([]ReviewedPack, len(reviewedPacks))
	for index, pack := range reviewedPacks {
		pack.Versions = slices.Clone(pack.Versions)
		out[index] = pack
	}
	return out
}

// ReviewedPackFor returns the pack that replaces an operator class on a
// vendor's GPU for one exact engine version, if one was reviewed.
func ReviewedPackFor(operator, vendor, engine, version string) (ReviewedPack, bool) {
	for _, pack := range ReviewedPacks() {
		if pack.Operator == operator && strings.EqualFold(pack.Vendor, vendor) && strings.EqualFold(pack.Engine, engine) && slices.Contains(pack.Versions, strings.TrimSpace(version)) {
			return pack, true
		}
	}
	return ReviewedPack{}, false
}

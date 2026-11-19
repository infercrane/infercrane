package kernelplanner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The registry must agree with the packs the lab actually loads.
func TestReviewedPacksMatchTheirPackFiles(t *testing.T) {
	for _, pack := range ReviewedPacks() {
		body, err := os.ReadFile(filepath.Join("..", "..", "tools", "kernel-lab", "packs", string(pack.Family), "pack.json"))
		if err != nil {
			t.Fatalf("%s: %v", pack.Family, err)
		}
		var file struct {
			Family          OperatorFamily      `json:"family"`
			Runtimes        []string            `json:"runtimes"`
			RuntimeVersions map[string][]string `json:"runtime_versions"`
		}
		if err = json.Unmarshal(body, &file); err != nil {
			t.Fatal(err)
		}
		if file.Family != pack.Family || !slices.Contains(file.Runtimes, pack.Engine) || !slices.Equal(file.RuntimeVersions[pack.Engine], pack.Versions) {
			t.Fatalf("registry %+v disagrees with pack.json %+v", pack, file)
		}
	}
}

func TestReviewedPackForNeedsTheExactEngineVersion(t *testing.T) {
	if _, ok := ReviewedPackFor("norm", "nvidia", "vllm", "0.22.1"); !ok {
		t.Fatal("the reviewed RMSNorm pack was not found")
	}
	for _, missing := range [][4]string{{"norm", "nvidia", "vllm", "0.23.0"}, {"norm", "amd", "vllm", "0.22.1"}, {"norm", "nvidia", "sglang", "0.5.12"}, {"moe-experts", "nvidia", "vllm", "0.22.1"}} {
		if pack, ok := ReviewedPackFor(missing[0], missing[1], missing[2], missing[3]); ok {
			t.Fatalf("%v matched %+v", missing, pack)
		}
	}
}

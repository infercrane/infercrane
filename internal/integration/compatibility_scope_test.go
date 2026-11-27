package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/support"
)

func TestNarrowedCompatibilityAppliesOnlyToItsTuple(t *testing.T) {
	entry := RuntimeCompatibility{Runtime: "vllm", Adapter: "kubernetes", Cloud: "kubernetes", Mode: ElasticMode, State: QualificationReal, Accelerators: []string{"NVIDIA-A10G"}, RuntimeVersion: "0.34.0", Evidence: "docs:x"}
	for _, test := range []struct {
		gpu, version string
		want         bool
	}{
		{"NVIDIA-A10G", "0.34.0", true},
		{"A10G", "0.34.0", true},
		{"nvidia a10g", "v0.34.0", true},
		{"NVIDIA-A10G-SHARED", "0.34.0", false},
		{"NVIDIA-L4", "0.34.0", false},
		{"", "0.34.0", false},
		{"NVIDIA-A10G", "0.22.1", false},
	} {
		if got := entry.Applies(test.gpu, test.version); got != test.want {
			t.Fatalf("Applies(%q, %q) = %v, want %v", test.gpu, test.version, got, test.want)
		}
	}
	general := RuntimeCompatibility{Runtime: "vllm", Adapter: "kubernetes", Cloud: "kubernetes", Mode: ElasticMode, State: QualificationSimulated, Evidence: "go:x"}
	if !general.Applies("NVIDIA-L4", "0.30.0") || !general.Applies("", "") {
		t.Fatal("an entry without scope must cover every GPU and version")
	}
}

func TestCompatibilityRegistersOneNarrowedEntryBesideTheGeneralOne(t *testing.T) {
	registry, err := V1Catalog()
	if err != nil {
		t.Fatal(err)
	}
	entries := registry.Snapshot().Compatibility
	scoped := RuntimeCompatibility{Runtime: "vllm", Adapter: "kubernetes", Cloud: "kubernetes", Mode: ElasticMode, State: QualificationReal, Accelerators: []string{"nvidia-a10g"}, RuntimeVersion: "0.34.0", Image: "vllm/vllm-openai@sha256:20fdb089454afbb324efa4110a2c025ff7b27cc1a086e07aa580b18df8ad1ce1", Evidence: "docs:x"}
	if err := registry.SetCompatibility(append(entries, scoped)...); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("a second entry for the same tuple must be refused: %v", err)
	}
	simulated := scoped
	simulated.State, simulated.Accelerators = QualificationSimulated, []string{"NVIDIA-L4"}
	if err := registry.SetCompatibility(append(entries, simulated)...); err == nil || !strings.Contains(err.Error(), "qualified run") {
		t.Fatalf("a narrowed entry must record a qualified run: %v", err)
	}
	other := scoped
	other.Accelerators = []string{"NVIDIA-L40S"}
	if err := registry.SetCompatibility(append(entries, other)...); err != nil {
		t.Fatalf("a narrowed entry for another GPU must register: %v", err)
	}
}

func TestCatalogQualifiesOnlyTheEKSA10GTuple(t *testing.T) {
	registry, err := V1Catalog()
	if err != nil {
		t.Fatal(err)
	}
	snapshot := registry.Snapshot()
	var real []RuntimeCompatibility
	for _, entry := range snapshot.Compatibility {
		if entry.Adapter == "kubernetes" && entry.State == QualificationReal {
			real = append(real, entry)
		}
	}
	if len(real) != 1 || real[0].Runtime != "vllm" || len(real[0].Accelerators) != 1 || real[0].Accelerators[0] != "NVIDIA-A10G" || real[0].RuntimeVersion != "0.34.0" {
		t.Fatalf("real-qualified Kubernetes entries: %+v", real)
	}
	// The image the run served is the one the runtime contract pins for the
	// version, so the entry cannot outlive a change of image.
	profile, err := registry.Runtime(support.DefaultRuntime)
	if err != nil {
		t.Fatal(err)
	}
	workload, err := profile.WorkloadFor("NVIDIA-A10G", real[0].RuntimeVersion)
	if err != nil || workload.Image != real[0].Image {
		t.Fatalf("pinned image %q (err %v), entry names %q", workload.Image, err, real[0].Image)
	}
	path := strings.TrimPrefix(real[0].Evidence, "docs:")
	if _, err := os.Stat(filepath.Join("..", "..", path)); err != nil {
		t.Fatalf("the entry's evidence %s: %v", path, err)
	}
	for gpu, want := range map[string]QualificationState{"NVIDIA-A10G": QualificationReal, "NVIDIA-L4": QualificationSimulated, "NVIDIA-A10G-SHARED": QualificationSimulated} {
		entry, found := SelectCompatibility(snapshot.Compatibility, "vllm", "kubernetes", ElasticMode, gpu, "0.34.0")
		if !found || entry.State != want {
			t.Fatalf("%s: %+v (found %v), want %s", gpu, entry, found, want)
		}
	}
	if entry, _ := SelectCompatibility(snapshot.Compatibility, "vllm", "kubernetes", ElasticMode, "NVIDIA-A10G", "0.22.1"); entry.State != QualificationSimulated {
		t.Fatalf("another engine version is not the qualified tuple: %+v", entry)
	}
}

func TestStrongestKeepsOneEntryPerRuntimeAdapterAndMode(t *testing.T) {
	general := RuntimeCompatibility{Runtime: "vllm", Adapter: "kubernetes", Cloud: "kubernetes", Mode: ElasticMode, State: QualificationSimulated}
	narrowed := general
	narrowed.State, narrowed.Accelerators = QualificationReal, []string{"NVIDIA-A10G"}
	dynamo := RuntimeCompatibility{Runtime: "vllm", Adapter: "kubernetes-dynamo", Cloud: "kubernetes", Mode: ElasticMode, State: QualificationSimulated}
	got := Strongest([]RuntimeCompatibility{general, dynamo, narrowed})
	if len(got) != 2 || got[0].State != QualificationReal || got[1].Adapter != "kubernetes-dynamo" {
		t.Fatalf("strongest: %+v", got)
	}
}

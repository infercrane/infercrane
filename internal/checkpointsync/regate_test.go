package checkpointsync

import (
	"slices"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/checkpointcatalog"
	"github.com/infercrane/infercrane/internal/support"
)

// A pin move reopens builds the old pin could not serve, and only their
// eligibility changes; a build that needs a release above the pin stays
// blocked, now naming the new pin.
func TestRegateAppliesTheEngineGateToTheCurrentPin(t *testing.T) {
	catalog, err := checkpointcatalog.Default()
	if err != nil {
		t.Fatal(err)
	}
	// The committed catalog is gated against the images pinned in this build;
	// after moving a pin, run go run ./tools/checkpoint-catalog-sync regate --write.
	if _, stale := Regate(catalog); len(stale) != 0 {
		t.Fatalf("%d committed entries are gated against another pin, first %+v; run go run ./tools/checkpoint-catalog-sync regate --write", len(stale), stale[0])
	}
	// An entry gated against an older pin moves; only its eligibility does.
	old := catalog
	old.Entries = slices.Clone(catalog.Entries)
	for index, entry := range old.Entries {
		if entry.Eligibility.State == checkpointcatalog.EligibilityCandidate {
			old.Entries[index].Eligibility = checkpointcatalog.Eligibility{State: checkpointcatalog.EligibilityCatalogOnly, Blockers: []string{"no reviewed runtime fits the pinned images: vllm needs 0.28.0 or newer, pinned 0.22.1"}}
			break
		}
	}
	regated, changes := Regate(old)
	if err := checkpointcatalog.Validate(regated); err != nil {
		t.Fatalf("regated catalog is invalid: %v", err)
	}
	if len(changes) != 1 || changes[0].After.State != checkpointcatalog.EligibilityCandidate {
		t.Fatalf("changes=%+v", changes)
	}
	if again, more := Regate(regated); len(more) != 0 || len(again.Entries) != len(regated.Entries) {
		t.Fatalf("regate is not idempotent: %d more changes", len(more))
	}
	byID := map[string]checkpointcatalog.Entry{}
	for _, entry := range catalog.Entries {
		byID[entry.ID] = entry
	}
	for index, entry := range regated.Entries {
		before := catalog.Entries[index]
		before.Eligibility = entry.Eligibility
		if entry.ID != before.ID || entry.Review.State != before.Review.State || entry.Revision != before.Revision || !slices.Equal(entry.Limitations, before.Limitations) {
			t.Fatalf("regate changed more than eligibility on %s", entry.ID)
		}
		for _, blocker := range entry.Eligibility.Blockers {
			if strings.Contains(blocker, "pinned images") && !strings.Contains(blocker, "pinned "+support.DefaultRuntimeVersion) {
				t.Fatalf("%s names a pin other than the current one: %q", entry.ID, blocker)
			}
		}
	}
	for _, change := range changes {
		if change.Before.State == change.After.State && slices.Equal(change.Before.Blockers, change.After.Blockers) {
			t.Fatalf("reported change without a change: %+v", change)
		}
		if _, ok := byID[change.ID]; !ok {
			t.Fatalf("change for unknown entry %s", change.ID)
		}
	}
	// A synthetic entry that needs a release above the current pin stays
	// blocked by the engine gate alone.
	entry := regated.Entries[0]
	entry.Review.State, entry.License.State = checkpointcatalog.ReviewApproved, checkpointcatalog.LicenseApproved
	entry.Kind, entry.Hardware = checkpointcatalog.KindQuantized, checkpointcatalog.Hardware{Vendor: checkpointcatalog.CUDAVendor, Architectures: []string{"hopper"}, ProductSupport: checkpointcatalog.SupportRunnable}
	entry.Runtimes, entry.EngineRequirements = []string{"vllm"}, []checkpointcatalog.EngineRequirement{{Engine: "vllm", State: checkpointcatalog.EngineMinimumVersion, MinimumVersion: "99.0.0"}}
	if entry.Quantization != nil {
		entry.Quantization.Tool = checkpointcatalog.QuantizationTools()[0]
	}
	entry.Artifact.ModelCardDigest = "sha256:" + strings.Repeat("a", 64)
	got := eligibility(entry)
	if want := "no reviewed runtime fits the pinned images: vllm needs 99.0.0 or newer, pinned " + support.DefaultRuntimeVersion; !slices.Equal(got.Blockers, []string{want}) {
		t.Fatalf("future-release entry: %+v", got)
	}
}

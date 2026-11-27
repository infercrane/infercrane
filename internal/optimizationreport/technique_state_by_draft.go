package optimizationreport

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/infercrane/infercrane/internal/benchmark"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/technique"
)

// StateByDraftProofSchema names the record both evidence items of the
// one-engine technique (optimizer.AdaptiveSpeculationOneEngine) are digests
// of.
const StateByDraftProofSchema = "infercrane.state-by-draft-proof/v1"

// StateByDraftProof is what the campaign itself recorded about a one-engine
// setup: the image and command its deployment named (kept for audit; behind
// a proxy they are the proxy's), and the patched engine's gauges in each of
// its measured runs, which AIPerf scraped from the engine's /metrics. Nothing
// in it comes from a harness or a log.
type StateByDraftProof struct {
	Schema   string            `json:"schema"`
	Image    string            `json:"image"`
	Command  string            `json:"command"`
	Identity string            `json:"identity"`
	Runs     []StateByDraftRun `json:"runs"`
}

// StateByDraftRun is one measured run of the setup.
type StateByDraftRun struct {
	// Window is "first" or "fresh-servers"; Run counts the setup's runs in it.
	Window string `json:"window"`
	Run    int    `json:"run"`
	// Gauges is whether every patched gauge was in the run's server metrics.
	// Stock vLLM has none of them.
	Gauges bool `json:"gauges"`
	// ReceiptVerified is whether the engine reported, at every scrape, that
	// it found the image's receipt and its patched files' sha256 as built.
	ReceiptVerified bool `json:"receipt_verified"`
	// MostReservingThree and MostReservingNone are the most running requests
	// reserving three and no draft tokens at one scrape; MostMixed is the most
	// of the smaller of those reserving three and fewer.
	MostReservingThree float64 `json:"most_reserving_three"`
	MostReservingNone  float64 `json:"most_reserving_none"`
	MostMixed          float64 `json:"most_mixed"`
}

// stateByDraftEvidence is the one-engine setup's evidence as the campaign
// recorded it, from the engine itself, so it holds on any deploy path, a
// proxy to a remote engine included. The receipt item needs every measured
// run's server metrics to carry the patched gauges and the engine's own
// receipt check at 1 at every scrape. The reservation-mix item needs that,
// and requests reserving three draft tokens and fewer running together in
// every measured run. A run without them leaves the item out, so the claim
// stays "not shown".
func stateByDraftEvidence(draft optimizer.DeploymentDraft, measured optimizationcampaign.MeasuredEvidence) []technique.EvidenceItem {
	proof := stateByDraftProof(draft, measured)
	if len(proof.Runs) == 0 {
		return nil
	}
	mixed := 0
	for _, run := range proof.Runs {
		if !run.Gauges || !run.ReceiptVerified {
			return nil
		}
		if run.MostMixed >= 1 {
			mixed++
		}
	}
	body, err := json.Marshal(proof)
	if err != nil {
		return nil
	}
	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	items := []technique.EvidenceItem{{Name: optimizer.EvidenceStateByDraftReceipt, Digest: digest,
		Summary: fmt.Sprintf("in all %d measured runs the engine reported its image's receipt and patched files' sha256 as built, and the patched gauges answered",
			len(proof.Runs))}}
	if mixed == len(proof.Runs) {
		most := 0.0
		for _, run := range proof.Runs {
			most = max(most, run.MostMixed)
		}
		items = append(items, technique.EvidenceItem{Name: optimizer.EvidenceReservationMix, Digest: digest,
			Summary: fmt.Sprintf("requests reserving three draft tokens and fewer ran together in all %d measured runs, up to %.0f of each at one scrape", len(proof.Runs), most)})
	}
	return items
}

func stateByDraftProof(draft optimizer.DeploymentDraft, measured optimizationcampaign.MeasuredEvidence) StateByDraftProof {
	proof := StateByDraftProof{Schema: StateByDraftProofSchema, Image: draft.Runtime.Workload.Image, Identity: optimizer.StateByDraftIdentity, Runs: []StateByDraftRun{}}
	if len(draft.Runtime.Workload.Command) > 0 {
		proof.Command = draft.Runtime.Workload.Command[0]
	}
	windows := []struct {
		name string
		runs []optimizationcampaign.RunSummary
	}{{"first", measured.Runs}}
	if measured.Confirmation != nil {
		windows = append(windows, struct {
			name string
			runs []optimizationcampaign.RunSummary
		}{"fresh-servers", measured.Confirmation.Runs})
	}
	for _, window := range windows {
		count := 0
		for _, run := range window.runs {
			if run.Arm != "candidate" {
				continue
			}
			count++
			proof.Runs = append(proof.Runs, stateByDraftRun(window.name, count, run.ServerMetrics))
		}
	}
	return proof
}

func stateByDraftRun(window string, index int, metrics *benchmark.ServerMetrics) StateByDraftRun {
	run := StateByDraftRun{Window: window, Run: index}
	if metrics == nil || !metrics.Collected {
		return run
	}
	most := func(name string) (float64, bool) {
		summary, found := metrics.Metrics[name]
		if !found {
			return 0, false
		}
		value, found := summary.Stats["max"]
		return value, found
	}
	run.Gauges = true
	for _, name := range append([]string{optimizer.StateByDraftMixedGauge, optimizer.StateByDraftReceiptGauge}, optimizer.StateByDraftReservingGauges...) {
		if _, found := most(name); !found {
			run.Gauges = false
		}
	}
	// A gauge that read 1 at every scrape: its smallest reading, or its mean
	// when the export kept no minimum.
	if receipt, found := metrics.Metrics[optimizer.StateByDraftReceiptGauge]; found {
		least, kept := receipt.Stats["min"]
		if !kept {
			least, kept = receipt.Stats["avg"]
		}
		run.ReceiptVerified = kept && least >= 1
	}
	run.MostMixed, _ = most(optimizer.StateByDraftMixedGauge)
	run.MostReservingThree, _ = most(optimizer.StateByDraftReservingGauges[2])
	run.MostReservingNone, _ = most(optimizer.StateByDraftReservingGauges[0])
	return run
}

// carriedDisclosures are the manifest's disclosures the proposed setup
// carried among its limitations: the record carries what the product showed
// with the setup, so a disclosure the product did not show leaves the claim
// "not shown".
func carriedDisclosures(manifest technique.Manifest, limitations []string) []string {
	var carried []string
	for _, disclosure := range manifest.Disclosures {
		for _, limitation := range limitations {
			if limitation == disclosure {
				carried = append(carried, disclosure)
				break
			}
		}
	}
	return carried
}

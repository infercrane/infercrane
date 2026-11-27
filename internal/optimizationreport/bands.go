package optimizationreport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/infercrane/infercrane/internal/bandverdict"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizer"
)

// BandStudySchema names the band study's response.
const BandStudySchema = "infercrane.dev/band-study/v1"

// A band study is one finished campaign per load: the same model and current
// setup, each campaign measuring its setups at its own load with the judge's
// interleaved control, fresh-server confirmation, guards and quality checks.
// The study reads each campaign's decision as that load band's verdict and
// says what holds across them. It decides nothing itself.
const (
	minStudyCampaigns = 2
	maxStudyCampaigns = 8
)

// ErrStudyMismatch is a set of campaigns that is not one study.
var ErrStudyMismatch = errors.New("the campaigns are not one band study")

// BandStudy is the verdict across campaigns, one per load.
type BandStudy struct {
	SchemaVersion string          `json:"schema_version"`
	Model         string          `json:"model"`
	Current       string          `json:"current"`
	Campaigns     []StudyCampaign `json:"campaigns"`
	// Setups describes each setup the bands name, keyed by its setup key: what
	// it serves, which is the same across campaigns whatever their candidate
	// IDs.
	Setups  map[string]StudySetup `json:"setups"`
	Verdict bandverdict.Verdict   `json:"verdict"`
}

// StudyCampaign is one campaign of the study and its band.
type StudyCampaign struct {
	ID      string `json:"id"`
	Band    string `json:"band"`
	Outcome string `json:"outcome"`
}

// StudySetup is what one setup serves.
type StudySetup struct {
	Label         string            `json:"label"`
	Engine        string            `json:"engine"`
	EngineVersion string            `json:"engine_version"`
	Args          []string          `json:"args"`
	Env           map[string]string `json:"env,omitempty"`
}

// LoadBandStudy reads 2 to 8 campaigns of one tenant and gives the study.
// domain.ErrNotFound means a campaign does not exist for the tenant;
// ErrStudyMismatch that the campaigns do not share a model, a current setup,
// an objective, and distinct loads.
func LoadBandStudy(ctx context.Context, store Store, tenant string, campaignIDs []string) (BandStudy, error) {
	if len(campaignIDs) < minStudyCampaigns || len(campaignIDs) > maxStudyCampaigns {
		return BandStudy{}, fmt.Errorf("%w: a band study reads %d to %d campaigns", ErrStudyMismatch, minStudyCampaigns, maxStudyCampaigns)
	}
	reports, campaigns := make([]Report, 0, len(campaignIDs)), make([]domain.OptimizationCampaign, 0, len(campaignIDs))
	for _, id := range campaignIDs {
		report, err := Load(ctx, store, tenant, id)
		if err != nil {
			return BandStudy{}, err
		}
		campaign, err := store.OptimizationCampaign(ctx, tenant, id)
		if err != nil {
			return BandStudy{}, err
		}
		reports, campaigns = append(reports, report), append(campaigns, campaign)
	}
	return BandStudyOf(reports, campaigns)
}

// BandStudyOf gives the study from loaded reports and their campaigns, in the
// same order. It does no I/O.
func BandStudyOf(reports []Report, campaigns []domain.OptimizationCampaign) (BandStudy, error) {
	study := BandStudy{SchemaVersion: BandStudySchema, Setups: map[string]StudySetup{}}
	var metric string
	loads := map[int]bool{}
	var bands []bandverdict.BandVerdict
	for index, report := range reports {
		keys := map[string]string{}
		for _, run := range campaigns[index].Candidates {
			key, setup, ok := setupKey(run)
			if !ok {
				continue
			}
			keys[run.ID] = key
			if _, seen := study.Setups[key]; !seen {
				setup.Label = labelOf(report, run.ID)
				study.Setups[key] = setup
			}
		}
		current := keys[report.Outcome.BaselineCandidateID]
		switch {
		case index == 0:
			study.Model, study.Current, metric = report.ModelIdentity, current, report.ObjectiveMetric
		case report.ModelIdentity != study.Model:
			return BandStudy{}, fmt.Errorf("%w: campaign %s serves %s, not %s", ErrStudyMismatch, report.CampaignID, report.ModelIdentity, study.Model)
		case current == "" || current != study.Current:
			return BandStudy{}, fmt.Errorf("%w: campaign %s starts from another setup", ErrStudyMismatch, report.CampaignID)
		case report.ObjectiveMetric != metric:
			return BandStudy{}, fmt.Errorf("%w: campaign %s ranks on %s, not %s", ErrStudyMismatch, report.CampaignID, report.ObjectiveMetric, metric)
		}
		band := campaignBand(report, keys)
		if band.Load > 0 {
			if loads[band.Load] {
				return BandStudy{}, fmt.Errorf("%w: two campaigns measure %d requests at once", ErrStudyMismatch, band.Load)
			}
			loads[band.Load] = true
		}
		bands = append(bands, band)
		study.Campaigns = append(study.Campaigns, StudyCampaign{ID: report.CampaignID, Band: band.Key, Outcome: report.Outcome.Kind})
	}
	if study.Current == "" {
		return BandStudy{}, fmt.Errorf("%w: the first campaign names no readable current setup", ErrStudyMismatch)
	}
	sort.SliceStable(bands, func(i, j int) bool {
		if bands[i].Kind != bands[j].Kind {
			return bands[i].Kind < bands[j].Kind
		}
		return bands[i].Load < bands[j].Load
	})
	study.Verdict = bandverdict.Summarize(metric, study.Current, bands)
	return study, nil
}

// campaignBand reads one campaign's decision as its band's verdict. Gains are
// each setup's change from the current setup on the objective in its own
// interleaved window, and the noise floor is the judge's.
func campaignBand(report Report, keys map[string]string) bandverdict.BandVerdict {
	kind := report.Workload.Profile
	if kind == "" {
		kind = report.Workload.Source
	}
	load := 0
	if report.Workload.Concurrency != nil {
		load = *report.Workload.Concurrency
	}
	band := bandverdict.BandVerdict{Key: fmt.Sprintf("%s/%d", kind, load), Kind: kind, Load: load, RepeatsSteady: true, Setups: []bandverdict.SetupResult{}}
	for _, candidate := range report.Candidates {
		key := keys[candidate.ID]
		if key == "" || candidate.ID == report.Outcome.BaselineCandidateID {
			continue
		}
		result := bandverdict.SetupResult{ID: key}
		if comparison := candidate.VersusBaseline; comparison != nil {
			change, noise := comparison.ChangePercent/100, comparison.NoiseFloorPercent/100
			result.Gain, result.Ahead = &change, comparison.Decision == DecisionImproved && candidate.ID == report.Outcome.WinnerCandidateID
			band.NoiseFloor = max(band.NoiseFloor, noise)
		}
		if candidate.Reason != "" && len(candidate.Guards) > 0 {
			result.HeldBack = candidate.Reason
		}
		band.Setups = append(band.Setups, result)
	}
	switch {
	case load < 1:
		band.Decision, band.Reason = bandverdict.DecisionUnknown, "The campaign recorded no load in requests at once."
	case report.Outcome.Kind == OutcomeWinnerSelected && keys[report.Outcome.WinnerCandidateID] != "":
		band.Decision, band.Winner = bandverdict.DecisionWinner, keys[report.Outcome.WinnerCandidateID]
		band.Reason = "The campaign's judge selected it at this load, confirmed on fresh servers: " + report.Outcome.Headline
	case report.Outcome.Kind == OutcomeBaselineKept:
		band.Decision, band.Reason = bandverdict.DecisionKeep, "The campaign's judge kept the current setup at this load: "+report.Outcome.Headline
	default:
		band.Decision, band.Reason = bandverdict.DecisionUnknown, fmt.Sprintf("The campaign has no decision at this load (%s).", strings.ReplaceAll(report.Outcome.Kind, "_", " "))
	}
	return band
}

// setupKey names what a candidate serves, so one setup has one key across
// campaigns whose candidate IDs differ: the model and revision, engine and
// version, flags, environment, and GPUs.
func setupKey(run domain.OptimizationCandidateRun) (string, StudySetup, bool) {
	var draft optimizer.DeploymentDraft
	if json.Unmarshal([]byte(run.DeploymentSpecJSON), &draft) != nil || draft.Model.ID == "" {
		return "", StudySetup{}, false
	}
	identity, _ := json.Marshal(struct {
		Model, Revision, Engine, Version, GPU string
		GPUCount                              int
		Args                                  []string
		Env                                   map[string]string
	}{draft.Model.ID, draft.Model.Revision, draft.Runtime.Engine, draft.Runtime.Version, draft.Resources.GPU, draft.Resources.GPUCount, draft.Runtime.Args, draft.Runtime.Env})
	sum := sha256.Sum256(identity)
	args := draft.Runtime.Args
	if args == nil {
		args = []string{}
	}
	return "setup-" + hex.EncodeToString(sum[:])[:12], StudySetup{Engine: draft.Runtime.Engine, EngineVersion: draft.Runtime.Version, Args: args, Env: draft.Runtime.Env}, true
}

func labelOf(report Report, candidateID string) string {
	for _, candidate := range report.Candidates {
		if candidate.ID == candidateID {
			return candidate.Label
		}
	}
	return ""
}

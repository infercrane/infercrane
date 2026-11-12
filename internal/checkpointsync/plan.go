package checkpointsync

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/infercrane/infercrane/internal/checkpointcatalog"
)

const (
	limitationUnmeasured = "Publisher-reported results are unmeasured priors; InferCrane has no workload, quality, cost, or release qualification for this build."
	speculatorBlocker    = "speculator: speculative-decoding candidates need acceptance and output-parity gates first"
)

// Result is a proposed catalog and the reviewable difference from the
// committed one.
type Result struct {
	Catalog checkpointcatalog.Catalog
	Diff    Diff
}

type Diff struct {
	FetchedAt      string
	SnapshotDigest string
	PolicyDigest   string
	Notes          []string
	Added          []checkpointcatalog.Entry
	Revised        []Revision
	Changed        []Change
	Removed        []Removal
	Unchanged      int
	Skipped        map[string][]string
}

type Revision struct {
	Repository string
	From       []checkpointcatalog.Entry
	To         []checkpointcatalog.Entry
}

type Change struct {
	Entry  checkpointcatalog.Entry
	Fields []string
}

type Removal struct {
	Entry  checkpointcatalog.Entry
	Reason string
}

type planner struct {
	snapshot *Snapshot
	policy   checkpointcatalog.Policy
	previous checkpointcatalog.Catalog
	skipped  map[string][]string
	removed  []Removal
}

// Plan builds the proposed catalog from a recorded snapshot. It never touches
// the network and produces identical output for identical inputs.
func Plan(snapshot *Snapshot, policy checkpointcatalog.Policy, policyDigest string, previous checkpointcatalog.Catalog, reviewedAt string) (Result, error) {
	digest, err := snapshot.Digest()
	if err != nil {
		return Result{}, err
	}
	p := &planner{snapshot: snapshot, policy: policy, previous: previous, skipped: map[string][]string{}}
	diff := Diff{FetchedAt: snapshot.Manifest.FetchedAt, SnapshotDigest: digest, PolicyDigest: policyDigest, Skipped: p.skipped}
	if snapshot.Manifest.PolicyDigest != policyDigest {
		diff.Notes = append(diff.Notes, "The policy changed after this snapshot was fetched; listing windows come from the snapshot.")
	}
	heads, unavailable, err := p.heads(&diff)
	if err != nil {
		return Result{}, err
	}
	revisions := map[string]map[string]struct{}{}
	add := func(repository, revision string) {
		if revisions[repository] == nil {
			revisions[repository] = map[string]struct{}{}
		}
		revisions[repository][strings.ToLower(revision)] = struct{}{}
	}
	for repository, head := range heads {
		add(repository, head.SHA)
	}
	for _, entry := range previous.Entries {
		add(entry.Repository, entry.Revision)
	}
	for _, value := range snapshot.Manifest.Pins {
		if repository, revision, found := strings.Cut(value, "@"); found {
			add(repository, revision)
		}
	}
	var entries []checkpointcatalog.Entry
	for _, repository := range sortedKeys(revisions) {
		built, buildErr := p.repository(repository, sortedKeys(revisions[repository]), heads, unavailable)
		if buildErr != nil {
			return Result{}, buildErr
		}
		entries = append(entries, built...)
	}
	sortEntries(entries)
	catalog := checkpointcatalog.Catalog{
		SchemaVersion: checkpointcatalog.SchemaVersion,
		ReviewedAt:    reviewedAt,
		Snapshot: checkpointcatalog.Snapshot{
			Source: snapshot.Manifest.Source + "/api/models", FetchedAt: snapshot.Manifest.FetchedAt,
			Digest: digest, PolicyDigest: policyDigest,
		},
		Entries: entries,
	}
	for _, publisher := range policy.Publishers {
		catalog.Publishers = append(catalog.Publishers, checkpointcatalog.Publisher{Organization: publisher.Organization, Name: publisher.Name, HardwareVendor: publisher.HardwareVendor, ReviewState: publisher.ReviewState})
	}
	sort.Slice(catalog.Publishers, func(i, j int) bool { return catalog.Publishers[i].Organization < catalog.Publishers[j].Organization })
	if err = checkpointcatalog.Validate(catalog); err != nil {
		return Result{}, fmt.Errorf("proposed catalog is invalid: %w", err)
	}
	compare(previous, catalog, p.removed, &diff)
	for reason := range diff.Skipped {
		sort.Strings(diff.Skipped[reason])
		diff.Skipped[reason] = slices.Compact(diff.Skipped[reason])
	}
	return Result{Catalog: catalog, Diff: diff}, nil
}

// heads returns the newest observed record for every repository the plan
// tracks, and the repositories the upstream reported as unavailable.
func (p *planner) heads(diff *Diff) (map[string]ModelInfo, map[string]int, error) {
	heads, unavailable := map[string]ModelInfo{}, map[string]int{}
	fetched := map[string]ManifestPublisher{}
	for _, publisher := range p.snapshot.Manifest.Publishers {
		fetched[strings.ToLower(publisher.Organization)] = publisher
	}
	for _, publisher := range p.policy.Publishers {
		window, found := fetched[strings.ToLower(publisher.Organization)]
		if !found {
			diff.Notes = append(diff.Notes, fmt.Sprintf("Publisher %s was not fetched in this snapshot; it gains no new entries.", publisher.Organization))
			continue
		}
		items, err := p.snapshot.Listing(window.Organization)
		if err != nil {
			return nil, nil, err
		}
		for _, item := range items {
			if !inWindow(item, checkpointcatalog.PublisherPolicy{CreatedAfter: window.CreatedAfter}) {
				continue
			}
			reason := screen(item)
			if excluded, found := p.policy.Excluded(item.ID, ""); found && reason == "" {
				reason = "excluded by policy: " + excluded
			}
			if reason != "" {
				p.skipped[reason] = append(p.skipped[reason], item.ID)
				continue
			}
			heads[item.ID] = item
		}
	}
	tracked := map[string]struct{}{}
	for _, entry := range p.previous.Entries {
		tracked[entry.Repository] = struct{}{}
	}
	for _, value := range p.snapshot.Manifest.Pins {
		if repository, _, found := strings.Cut(value, "@"); found {
			tracked[repository] = struct{}{}
		}
	}
	for _, repository := range sortedKeys(tracked) {
		if _, listed := heads[repository]; listed {
			continue
		}
		head, found, err := p.snapshot.Head(repository)
		if err != nil {
			return nil, nil, err
		}
		switch {
		case found && head.Status != 0:
			unavailable[repository] = head.Status
		case found && screen(head) == "":
			heads[repository] = head
		}
	}
	return heads, unavailable, nil
}

func (p *planner) repository(repository string, revisions []string, heads map[string]ModelInfo, unavailable map[string]int) ([]checkpointcatalog.Entry, error) {
	var previous []checkpointcatalog.Entry
	for _, entry := range p.previous.Entries {
		if strings.EqualFold(entry.Repository, repository) {
			previous = append(previous, entry)
		}
	}
	if status, gone := unavailable[repository]; gone {
		for _, entry := range previous {
			p.removed = append(p.removed, Removal{Entry: entry, Reason: fmt.Sprintf("repository unavailable upstream (status %d)", status)})
		}
		return nil, nil
	}
	head, hasHead := heads[repository]
	var headEntry *checkpointcatalog.Entry
	var pinned []checkpointcatalog.Entry
	for _, revision := range revisions {
		info, found, err := p.snapshot.Info(repository, revision)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("snapshot is incomplete: no record for %s@%s", repository, revision)
		}
		if info.Status != 0 {
			p.dropPrevious(previous, revision, fmt.Sprintf("revision unavailable upstream (status %d)", info.Status))
			continue
		}
		entry, reason := p.build(info, p.snapshot.Files(repository, revision))
		if reason != "" {
			if hasHead && strings.EqualFold(head.SHA, revision) {
				p.skipped[reason] = append(p.skipped[reason], repository)
			}
			p.dropPrevious(previous, revision, reason)
			continue
		}
		if hasHead && strings.EqualFold(head.SHA, revision) {
			headEntry = &entry
		} else {
			pinned = append(pinned, entry)
		}
	}
	if headEntry == nil {
		for index := range pinned {
			pinned[index].Eligibility = eligibility(pinned[index])
		}
		return pinned, nil
	}
	if reviewed := previousRevision(previous, headEntry.Revision); reviewed != nil {
		if fields := materialChanges(*reviewed, *headEntry); len(fields) > 0 {
			headEntry.Review.State = checkpointcatalog.ReviewPending
			headEntry.Review.Reasons = append(headEntry.Review.Reasons, fmt.Sprintf("new revision changes %s relative to reviewed revision %s", strings.Join(fields, ", "), short(reviewed.Revision)))
		}
	}
	headEntry.Eligibility = eligibility(*headEntry)
	out := []checkpointcatalog.Entry{*headEntry}
	for _, entry := range pinned {
		entry.Eligibility = eligibility(entry)
		if headEntry.Eligibility.State == checkpointcatalog.EligibilityCandidate || entry.Eligibility.State != checkpointcatalog.EligibilityCandidate {
			p.dropPrevious(previous, entry.Revision, "replaced by revision "+short(headEntry.Revision))
			continue
		}
		entry.RevisionState, entry.SupersededBy = checkpointcatalog.RevisionSuperseded, headEntry.Revision
		entry.Limitations = append(entry.Limitations, fmt.Sprintf("A newer revision (%s) exists but is not a candidate; this pinned revision stays until it is.", short(headEntry.Revision)))
		out = append(out, entry)
	}
	return out, nil
}

func (p *planner) dropPrevious(previous []checkpointcatalog.Entry, revision, reason string) {
	for _, entry := range previous {
		if strings.EqualFold(entry.Revision, revision) {
			p.removed = append(p.removed, Removal{Entry: entry, Reason: reason})
		}
	}
}

// build derives one entry from the recorded revision. Review starts from the
// policy; eligibility is computed once retention is known.
func (p *planner) build(info ModelInfo, files fileReader) (checkpointcatalog.Entry, string) {
	publisher, known := p.policy.Publisher(info.organization())
	if !known {
		return checkpointcatalog.Entry{}, "publisher is not in the reviewed policy"
	}
	if reason, excluded := p.policy.Excluded(info.ID, info.SHA); excluded {
		return checkpointcatalog.Entry{}, "excluded by policy: " + reason
	}
	class, reason := classify(info, files)
	if reason != "" {
		return checkpointcatalog.Entry{}, reason
	}
	readme, hasReadme := files("README.md")
	revision := strings.ToLower(info.SHA)
	entry := checkpointcatalog.Entry{
		ID: entryID(info.ID, revision), Kind: class.Kind, Publisher: publisher.Organization,
		Repository: info.ID, Revision: revision, RevisionState: checkpointcatalog.RevisionCurrent,
		CreatedAt: info.CreatedAt, LastModified: info.LastModified,
		SourceURL: "https://huggingface.co/" + info.ID,
	}
	scheme := class.Scheme
	if class.Kind == checkpointcatalog.KindSpeculator {
		scheme = class.Speculator
		entry.Speculator = &checkpointcatalog.Speculator{Method: class.Speculator, Targets: class.Bases}
	} else {
		entry.Quantization = &checkpointcatalog.Quantization{Tool: class.Tool, ToolVersion: class.ToolVersion, Method: class.Method, Scheme: class.Scheme}
	}
	format, formatKnown := publisher.Format(class.Kind, class.Tool, scheme)
	target := checkpointcatalog.HardwareTarget{Vendor: publisher.HardwareVendor}
	entry.Review.State = checkpointcatalog.ReviewApproved
	if publisher.ReviewState != checkpointcatalog.ReviewApproved {
		entry.Review.State = checkpointcatalog.ReviewPending
		entry.Review.Reasons = append(entry.Review.Reasons, fmt.Sprintf("new publisher %s requires review", publisher.Organization))
	}
	formatName := class.Kind + " " + strings.TrimPrefix(class.Tool+"/", "/") + scheme
	switch {
	case !formatKnown:
		entry.Review.State = checkpointcatalog.ReviewPending
		entry.Review.Reasons = append(entry.Review.Reasons, fmt.Sprintf("new format %s for %s requires review", formatName, publisher.Organization))
	case format.ReviewState != checkpointcatalog.ReviewApproved:
		entry.Review.State = checkpointcatalog.ReviewPending
		entry.Review.Reasons = append(entry.Review.Reasons, fmt.Sprintf("format %s for %s is pending review", formatName, publisher.Organization))
		target, entry.Runtimes = format.Hardware, append([]string(nil), format.Runtimes...)
	default:
		target, entry.Runtimes = format.Hardware, append([]string(nil), format.Runtimes...)
	}
	if entry.Runtimes == nil {
		entry.Runtimes = []string{}
	}
	entry.Hardware = checkpointcatalog.Hardware{Vendor: target.Vendor, Architectures: append([]string{}, target.Architectures...), ProductSupport: checkpointcatalog.SupportUnknown}
	switch {
	case target.Vendor != "nvidia":
		entry.Hardware.ProductSupport = checkpointcatalog.SupportUnsupported
	case p.policy.Runnable(target):
		entry.Hardware.ProductSupport = checkpointcatalog.SupportRunnable
	}
	p.link(&entry, class, readme)
	tag := info.license()
	spdx, state := p.policy.License(info.ID, tag)
	entry.License = checkpointcatalog.License{Tag: tag, SPDX: spdx, State: state}
	if name := info.licenseFile(); name != "" {
		if raw, found := files(name); found {
			entry.License.Digest = digestBytes(raw)
		}
	}
	manifest, count, size := manifestDigest(info)
	entry.Artifact = checkpointcatalog.Artifact{Format: class.Format, ManifestDigest: manifest, FileCount: count, SizeBytes: size}
	if hasReadme {
		entry.Artifact.ModelCardDigest = digestBytes(readme)
		entry.PublisherPriors = priorTables(readme, revision)
	}
	entry.EngineRequirements = append(engineRequirements(readme, p.policy), p.policy.EngineBlocks(info.ID, revision)...)
	sortRequirements(entry.EngineRequirements)
	entry.Limitations = limitations(entry, info, hasReadme)
	return entry, ""
}

// link connects a build to its base model and records the base revision a
// screening campaign should pin.
func (p *planner) link(entry *checkpointcatalog.Entry, class classification, readme []byte) {
	repository := class.Bases[0]
	entry.Base = checkpointcatalog.Base{Repository: repository, Lineage: checkpointcatalog.LineagePublisherDeclared}
	if class.Kind == checkpointcatalog.KindQuantized {
		if declared := declaredBaseRevision(readme, repository); declared != "" {
			entry.Base.DeclaredRevision, entry.Base.Lineage = declared, checkpointcatalog.LineageExact
			entry.Base.ScreeningRevision = declared
			entry.Base.ScreeningRevisionSource = "https://huggingface.co/" + repository + "/commit/" + declared
			return
		}
	}
	info, found, err := p.snapshot.Base(repository)
	if err == nil && found && info.Status == 0 && checkpointcatalog.ImmutableRevision(info.SHA) {
		entry.Base.ScreeningRevision = strings.ToLower(info.SHA)
		entry.Base.ScreeningRevisionSource = "https://huggingface.co/api/models/" + repository
	}
}

func limitations(entry checkpointcatalog.Entry, info ModelInfo, hasReadme bool) []string {
	out := []string{limitationUnmeasured}
	switch {
	case entry.Base.Lineage == checkpointcatalog.LineageExact:
		out = append(out, "The model card states the exact base revision.")
	case entry.Base.ScreeningRevision != "":
		out = append(out, "The publisher declares the base repository but not an exact base revision; the screening revision is the base head in the snapshot.")
	default:
		out = append(out, fmt.Sprintf("The declared base repository %s was not found on Hugging Face; no screening revision is recorded.", entry.Base.Repository))
	}
	if entry.Hardware.ProductSupport == checkpointcatalog.SupportUnsupported {
		out = append(out, fmt.Sprintf("Targets %s hardware, which InferCrane cannot run yet.", entry.Hardware.Vendor))
	}
	for _, requirement := range entry.EngineRequirements {
		switch requirement.State {
		case checkpointcatalog.EngineUnreleasedChange:
			out = append(out, fmt.Sprintf("%s needs %s, which is not in a reviewed release.", requirement.Engine, requirement.PullRequest))
		case checkpointcatalog.EngineBlockedByReview:
			out = append(out, fmt.Sprintf("%s is blocked by review: %s", requirement.Engine, requirement.Note))
		}
	}
	if entry.License.State != checkpointcatalog.LicenseApproved {
		out = append(out, fmt.Sprintf("License %q must be resolved before a customer trial.", entry.License.Tag))
	}
	if info.licenseFile() == "" {
		out = append(out, "No license file at the pinned revision; the license comes from the model card metadata.")
	}
	if !hasReadme {
		out = append(out, "No model card at the pinned revision.")
	}
	if info.gated() {
		out = append(out, "The repository is gated; accept its terms before a trial.")
	}
	return out
}

func eligibility(entry checkpointcatalog.Entry) checkpointcatalog.Eligibility {
	var blockers []string
	if entry.Review.State != checkpointcatalog.ReviewApproved {
		blockers = append(blockers, "review pending")
	}
	if entry.Kind == checkpointcatalog.KindSpeculator {
		blockers = append(blockers, speculatorBlocker)
	}
	switch entry.Hardware.ProductSupport {
	case checkpointcatalog.SupportUnsupported:
		blockers = append(blockers, fmt.Sprintf("targets %s hardware the product cannot run yet", entry.Hardware.Vendor))
	case checkpointcatalog.SupportUnknown:
		blockers = append(blockers, "target hardware is not reviewed")
	}
	if entry.License.State != checkpointcatalog.LicenseApproved {
		blockers = append(blockers, fmt.Sprintf("license %q is unresolved", entry.License.Tag))
	}
	if entry.Quantization != nil && !slices.Contains(checkpointcatalog.QuantizationTools(), entry.Quantization.Tool) {
		blockers = append(blockers, fmt.Sprintf("tool %s cannot seed product candidates yet", entry.Quantization.Tool))
	}
	if entry.Artifact.ModelCardDigest == "" {
		blockers = append(blockers, "no model card at the pinned revision")
	}
	if len(entry.Runtimes) == 0 {
		blockers = append(blockers, "no reviewed runtime")
	} else if !slices.ContainsFunc(entry.Runtimes, func(runtime string) bool { return runtimeOpen(entry.EngineRequirements, runtime) }) {
		blockers = append(blockers, "every reviewed runtime needs an unreleased or blocked engine change")
	}
	if len(blockers) == 0 {
		return checkpointcatalog.Eligibility{State: checkpointcatalog.EligibilityCandidate}
	}
	return checkpointcatalog.Eligibility{State: checkpointcatalog.EligibilityCatalogOnly, Blockers: blockers}
}

func runtimeOpen(requirements []checkpointcatalog.EngineRequirement, runtime string) bool {
	for _, requirement := range requirements {
		if strings.EqualFold(requirement.Engine, runtime) && requirement.State != checkpointcatalog.EngineMinimumVersion {
			return false
		}
	}
	return true
}

// previousRevision returns the newest committed revision of a repository when
// the observed head is a revision the catalog has not seen.
func previousRevision(previous []checkpointcatalog.Entry, revision string) *checkpointcatalog.Entry {
	var latest *checkpointcatalog.Entry
	for index := range previous {
		if strings.EqualFold(previous[index].Revision, revision) {
			return nil
		}
		if latest == nil || previous[index].LastModified > latest.LastModified {
			latest = &previous[index]
		}
	}
	return latest
}

// materialChanges lists the fields whose change on a new revision needs a
// human decision instead of the automated re-review.
func materialChanges(old, current checkpointcatalog.Entry) []string {
	var fields []string
	if old.Kind != current.Kind {
		fields = append(fields, "kind")
	}
	if !strings.EqualFold(old.Base.Repository, current.Base.Repository) {
		fields = append(fields, "base repository")
	}
	if scheme(old) != scheme(current) {
		fields = append(fields, "format")
	}
	if old.Hardware.Vendor != current.Hardware.Vendor {
		fields = append(fields, "hardware vendor")
	}
	if old.License.Tag != current.License.Tag {
		fields = append(fields, "license")
	}
	return fields
}

func scheme(entry checkpointcatalog.Entry) string {
	if entry.Quantization != nil {
		return entry.Quantization.Tool + "/" + entry.Quantization.Scheme
	}
	if entry.Speculator != nil {
		return "speculator/" + entry.Speculator.Method
	}
	return ""
}

func compare(previous, proposed checkpointcatalog.Catalog, removed []Removal, diff *Diff) {
	before, after := byRepository(previous.Entries), byRepository(proposed.Entries)
	for _, repository := range sortedKeys(after) {
		old, existed := before[repository]
		current := after[repository]
		switch {
		case !existed:
			diff.Added = append(diff.Added, current...)
		case revisionsOf(old) != revisionsOf(current):
			diff.Revised = append(diff.Revised, Revision{Repository: current[0].Repository, From: old, To: current})
		default:
			prior := map[string]checkpointcatalog.Entry{}
			for _, entry := range old {
				prior[entry.Revision] = entry
			}
			for _, entry := range current {
				if fields := changedFields(prior[entry.Revision], entry); len(fields) > 0 {
					diff.Changed = append(diff.Changed, Change{Entry: entry, Fields: fields})
				} else {
					diff.Unchanged++
				}
			}
		}
	}
	reported := map[string]struct{}{}
	for _, removal := range removed {
		if _, kept := after[strings.ToLower(removal.Entry.Repository)]; kept {
			continue
		}
		key := removal.Entry.Repository + "@" + removal.Entry.Revision
		if _, seen := reported[key]; !seen {
			diff.Removed = append(diff.Removed, removal)
			reported[key] = struct{}{}
		}
	}
	for _, repository := range sortedKeys(before) {
		if _, kept := after[repository]; kept {
			continue
		}
		for _, entry := range before[repository] {
			if _, seen := reported[entry.Repository+"@"+entry.Revision]; !seen {
				diff.Removed = append(diff.Removed, Removal{Entry: entry, Reason: "no longer observed for a reviewed publisher"})
			}
		}
	}
	sort.Slice(diff.Removed, func(i, j int) bool {
		return diff.Removed[i].Entry.Repository+diff.Removed[i].Entry.Revision < diff.Removed[j].Entry.Repository+diff.Removed[j].Entry.Revision
	})
}

func byRepository(entries []checkpointcatalog.Entry) map[string][]checkpointcatalog.Entry {
	out := map[string][]checkpointcatalog.Entry{}
	for _, entry := range entries {
		key := strings.ToLower(entry.Repository)
		out[key] = append(out[key], entry)
	}
	for key := range out {
		sortEntries(out[key])
	}
	return out
}

func revisionsOf(entries []checkpointcatalog.Entry) string {
	var revisions []string
	for _, entry := range entries {
		revisions = append(revisions, entry.Revision)
	}
	sort.Strings(revisions)
	return strings.Join(revisions, ",")
}

func changedFields(old, current checkpointcatalog.Entry) []string {
	var a, b map[string]json.RawMessage
	left, _ := json.Marshal(old)
	right, _ := json.Marshal(current)
	_ = json.Unmarshal(left, &a)
	_ = json.Unmarshal(right, &b)
	keys := map[string]struct{}{}
	for key := range a {
		keys[key] = struct{}{}
	}
	for key := range b {
		keys[key] = struct{}{}
	}
	var fields []string
	for _, key := range sortedKeys(keys) {
		if string(a[key]) != string(b[key]) {
			fields = append(fields, key)
		}
	}
	return fields
}

func sortEntries(entries []checkpointcatalog.Entry) {
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if !strings.EqualFold(a.Repository, b.Repository) {
			return strings.ToLower(a.Repository) < strings.ToLower(b.Repository)
		}
		if a.RevisionState != b.RevisionState {
			return a.RevisionState == checkpointcatalog.RevisionCurrent
		}
		return a.Revision < b.Revision
	})
}

var slugSeparators = regexp.MustCompile(`[^a-z0-9]+`)

// entryID keeps the legacy shape: <org>-<name without dots>-<revision prefix>.
func entryID(repository, revision string) string {
	organization, name, _ := strings.Cut(strings.ToLower(repository), "/")
	slug := strings.Trim(slugSeparators.ReplaceAllString(strings.ReplaceAll(name, ".", ""), "-"), "-")
	return slugSeparators.ReplaceAllString(organization, "") + "-" + slug + "-" + revision[:8]
}

func short(revision string) string {
	if len(revision) > 12 {
		return revision[:12]
	}
	return revision
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

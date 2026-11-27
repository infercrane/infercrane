package optimizer

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// A diagnosis ranks single techniques, and a ranking of single techniques
// cannot see what it did not choose: speculation that pays at a light load
// can cost at a heavy one, a draft length predicted from the cache room can
// be one step off, a pair of changes can beat each change alone, and servers
// of one setup can differ by more than the judge allows. So every
// diagnosis-ordered list also carries setups that test the plan itself, right
// after the diagnosis's first choice or after its last:
//
//   - right after the first choice, the starting setup without its
//     speculation, when it speculates (the publisher's plain command when the
//     starting setup is the publisher's command with its documented option),
//     and a copy of the starting setup, which differs from it only by its
//     machine;
//   - after the last diagnosed choice, so a limit on setups cuts them first,
//     the draft length next to the one chosen, unless the cache room the
//     engine reported predicts it holds fewer requests than the server runs
//     now;
//   - the chosen speculation together with the chosen precision change, when
//     the diagnosis chose both: lower precision changes which speculation
//     wins.
//
// They carry no diagnosis priority: their reasons say why they are measured.

// Reason roles of the setups every list carries besides the diagnosed ones.
const (
	// ReasonReference tests the diagnosis's own choice: the setup without
	// speculation, the neighbouring draft length, or a speculation and
	// precision pair.
	ReasonReference = "reference"
	// ReasonCopy is the starting setup with only a sampling seed added. It
	// differs from the starting setup only by its machine, so it measures the
	// machine difference, and a copy that wins says the run cannot separate
	// setups.
	ReasonCopy = "copy"
)

// Profiles of the standing setups.
const (
	// planCheckFeature marks a standing setup; its state names the check.
	planCheckFeature  = "plan_check"
	standingNeighbour = "reference-draft-length"
	standingPair      = "reference-speculation-precision"
	standingCopy      = "copy-of-starting-setup"
	maxDraftTokens    = 7
)

type standing struct {
	candidate Candidate
	reason    CandidateReason
	// last places it after every diagnosed setup instead of after the first.
	last bool
}

// standingCandidates derives the setups every list carries. first is the
// diagnosis's first choice and ranked every diagnosed setup in order; nothing
// is derived when the diagnosis chose nothing to change.
func standingCandidates(control Candidate, ranked []rankedCandidate, diagnosis Diagnosis, request Request, room *CacheRoom) ([]standing, error) {
	if len(ranked) == 0 {
		return nil, nil
	}
	var out []standing
	// add derives one standing setup. changes names the change: its id
	// becomes the setup's profile, and its risk class decides the evidence the
	// setup needs (a setup that can change answers needs a task score).
	add := func(changes technique, args []string, role, label, summary string, last bool) error {
		for _, existing := range ranked {
			if sameArgs(existing.candidate.Deployment.Runtime.Args, args) {
				return nil
			}
		}
		for _, existing := range out {
			if sameArgs(existing.candidate.Deployment.Runtime.Args, args) {
				return nil
			}
		}
		candidate, err := diagnosisCandidate(control, changes, args, nil, request)
		if err != nil {
			return err
		}
		// It is not a diagnosed technique, so it is not marked as one.
		for index, feature := range candidate.Features {
			if feature.Name == "diagnosis_technique" {
				candidate.Features[index].Name = planCheckFeature
			}
		}
		out = append(out, standing{candidate: candidate, last: last, reason: CandidateReason{Role: role, Label: label, ExpectedBenefit: "check", Rules: []string{}, Evidence: []ReasonEvidence{}, Summary: summary}})
		return nil
	}

	if args, why := deriveArgs(control, techniques[TechniqueSpeculationOff], diagnosis, request); why == "" {
		if err := add(techniques[TechniqueSpeculationOff], args, ReasonReference, "The starting setup without its speculation", "Measured whatever the diagnosis chose: speculation that pays at a light load can cost at a heavy one. When the starting setup is the publisher's command with its documented speculation option, this is the publisher's plain command.", false); err != nil {
			return nil, err
		}
	}
	if _, builtIn := reviewedEngineVersions[control.Deployment.Runtime.Engine]; builtIn && control.Deployment.Runtime.Workload.Empty() && control.Deployment.Serving.Empty() && !hasFlag(control.Deployment.Runtime.Args, "--seed") {
		args := append(append([]string(nil), control.Deployment.Runtime.Args...), "--seed", "1")
		if err := add(technique{id: standingCopy}, args, ReasonCopy, "A copy of the starting setup (must not win)", "The starting setup with only a sampling seed added: it differs from the starting setup only by its machine. Its difference measures how far two servers of one setup differ, and every other setup's interval is widened by it; if it wins, the run cannot separate setups.", false); err != nil {
			return nil, err
		}
	}
	var speculation, precision *rankedCandidate
	for index := range ranked {
		entry := &ranked[index]
		// Only setups derived from the starting setup: a catalog setup can
		// differ from it in more than its flags.
		switch {
		case !entry.derived:
		case speculation == nil && entry.choice.technique.speculative:
			speculation = entry
		case precision == nil && entry.choice.technique.changesPrecision && !entry.choice.technique.speculative:
			precision = entry
		}
	}
	if speculation != nil {
		chosen, ok := draftTokens(speculation.candidate.Deployment.Runtime.Args)
		current := 0
		if room != nil {
			current = room.CurrentDrafts
		}
		if ok {
			if neighbour, why := neighbourDrafts(chosen, current, room); why == "" {
				args, _ := withDraftTokens(speculation.candidate.Deployment.Runtime.Args, neighbour)
				summary := fmt.Sprintf("The diagnosis chose %s; the draft length next to it is measured too, because a draft length predicted from the cache room alone can be one step off.", draftWords(chosen))
				if err := add(technique{id: fmt.Sprintf("%s-%d", standingNeighbour, neighbour), speculative: true}, args, ReasonReference, "Speculation with "+draftWords(neighbour), summary, true); err != nil {
					return nil, err
				}
			}
		}
		if precision != nil {
			if args, changed := withArgs(speculation.candidate.Deployment.Runtime.Args, techniques[precision.choice.technique.id].args[control.Deployment.Runtime.Engine]); changed {
				label := speculation.choice.technique.label + " with " + strings.ToLower(precision.choice.technique.label)
				summary := "The diagnosis chose both changes. Lower precision changes which speculation wins and how much room the cache has, so the pair is measured as a setup of its own."
				if err := add(technique{id: standingPair, speculative: true, changesPrecision: true}, args, ReasonReference, label, summary, true); err != nil {
					return nil, err
				}
			}
		}
	}
	return out, nil
}

// appendStanding adds the standing setups placed first or last, each with
// its reason.
func appendStanding(ordered []Candidate, standing []standing, last bool) []Candidate {
	for _, extra := range standing {
		if extra.last != last {
			continue
		}
		candidate, reason := extra.candidate, extra.reason
		candidate.Reason = &reason
		ordered = append(ordered, candidate)
	}
	return ordered
}

// neighbourDrafts is the draft length next to chosen that is measured with
// it: the one between the starting setup's length and the chosen one when
// there is one, else the shorter. It is left out when it is the starting
// setup's length or none at all (both are measured anyway), or when the cache
// room predicts it holds fewer requests than the server runs now.
func neighbourDrafts(chosen, current int, room *CacheRoom) (int, string) {
	options := []int{}
	switch {
	case current > chosen:
		options = append(options, chosen+1, chosen-1)
	default:
		options = append(options, chosen-1, chosen+1)
	}
	for _, drafts := range options {
		if drafts < 1 || drafts > maxDraftTokens || drafts == current {
			continue
		}
		if room != nil {
			for _, length := range room.DraftLengths {
				if length.Drafts == drafts && length.Room < room.Running {
					return 0, fmt.Sprintf("the cache room predicts %s hold %.0f requests, fewer than the %.0f running now", draftWords(drafts), length.Room, room.Running)
				}
			}
		}
		return drafts, ""
	}
	return 0, "no other draft length to measure"
}

func draftWords(drafts int) string {
	if drafts == 1 {
		return "one draft token"
	}
	return fmt.Sprintf("%d draft tokens", drafts)
}

// draftTokens reads num_speculative_tokens from a --speculative-config.
func draftTokens(args []string) (int, bool) {
	value, ok := flagValue(args, "--speculative-config")
	if !ok {
		return 0, false
	}
	var config map[string]any
	if json.Unmarshal([]byte(value), &config) != nil {
		return 0, false
	}
	tokens, ok := config["num_speculative_tokens"].(float64)
	if !ok || tokens < 1 || tokens != float64(int(tokens)) {
		return 0, false
	}
	return int(tokens), true
}

// withDraftTokens sets num_speculative_tokens in the --speculative-config,
// keeping every other field and the flag's place.
func withDraftTokens(args []string, drafts int) ([]string, bool) {
	out := append([]string(nil), args...)
	for index, arg := range out {
		flag, value, inline := strings.Cut(arg, "=")
		if flag != "--speculative-config" {
			continue
		}
		if !inline {
			if index+1 >= len(out) {
				return nil, false
			}
			value = out[index+1]
		}
		var config map[string]any
		if json.Unmarshal([]byte(value), &config) != nil {
			return nil, false
		}
		config["num_speculative_tokens"] = drafts
		encoded := encodeSpeculationConfig(config)
		if inline {
			out[index] = flag + "=" + encoded
		} else {
			out[index+1] = encoded
		}
		return out, true
	}
	return nil, false
}

// encodeSpeculationConfig writes method first and the draft count second, as
// the reviewed flags do, then any other field in name order.
func encodeSpeculationConfig(config map[string]any) string {
	parts := []string{}
	write := func(key string) {
		if value, ok := config[key]; ok {
			encoded, _ := json.Marshal(value)
			parts = append(parts, strconv.Quote(key)+":"+string(encoded))
			delete(config, key)
		}
	}
	write("method")
	write("num_speculative_tokens")
	rest := make([]string, 0, len(config))
	for key := range config {
		rest = append(rest, key)
	}
	sort.Strings(rest)
	for _, key := range rest {
		write(key)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func flagValue(args []string, name string) (string, bool) {
	for index, arg := range args {
		flag, value, inline := strings.Cut(arg, "=")
		if flag != name {
			continue
		}
		if inline {
			return value, true
		}
		if index+1 < len(args) {
			return args[index+1], true
		}
	}
	return "", false
}

func hasFlag(args []string, name string) bool {
	_, ok := flagValue(args, name)
	return ok
}

func sameArgs(a, b []string) bool {
	return strings.Join(a, "\x00") == strings.Join(b, "\x00")
}

package technique

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// StaticSchemaV1 is a technique written as a file: a manifest and fixed
// changes to the starting setup's engine flags. It needs no code, so anyone
// can add an engine option this way. It builds no artifact and serves one
// setup at every load.
const StaticSchemaV1 = "infercrane.dev/technique-static/v1"

// StaticFile is the file form of a static technique.
type StaticFile struct {
	SchemaVersion string   `json:"schema_version"`
	Manifest      Manifest `json:"manifest"`
	Changes       []Change `json:"changes"`
}

// Change is one candidate: flags to set (replacing a value the starting
// setup has), flags to remove, and why it is worth measuring.
type Change struct {
	Label  string   `json:"label"`
	Reason string   `json:"reason"`
	Set    []Flag   `json:"set,omitempty"`
	Remove []string `json:"remove,omitempty"`
	// Prediction is optional and stated in the manifest's metric.
	Prediction *Prediction `json:"prediction,omitempty"`
}

// Flag is one engine flag. An empty Value is a switch with no value.
type Flag struct {
	Name  string `json:"name"`
	Value string `json:"value,omitempty"`
}

type static struct {
	manifest Manifest
	changes  []Change
}

const maxStaticChanges = 16

// LoadStatic reads and validates a static technique file.
func LoadStatic(body []byte) (Technique, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var file StaticFile
	if err := decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("decode static technique: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("static technique file holds more than one JSON value")
	}
	if file.SchemaVersion != StaticSchemaV1 {
		return nil, fmt.Errorf("static technique schema must be %q", StaticSchemaV1)
	}
	if err := file.Manifest.Validate(); err != nil {
		return nil, err
	}
	if file.Manifest.Artifact != nil || file.Manifest.Runtime.Policy != RuntimeStatic {
		return nil, fmt.Errorf("technique %s: a static technique builds no artifact and serves one setup at every load", file.Manifest.ID)
	}
	if len(file.Changes) == 0 || len(file.Changes) > maxStaticChanges {
		return nil, fmt.Errorf("technique %s must list 1 to %d changes", file.Manifest.ID, maxStaticChanges)
	}
	for _, change := range file.Changes {
		if strings.TrimSpace(change.Label) == "" || strings.TrimSpace(change.Reason) == "" || len(change.Set)+len(change.Remove) == 0 {
			return nil, fmt.Errorf("technique %s: each change has a label, a reason, and a flag to set or remove", file.Manifest.ID)
		}
		for _, flag := range change.Set {
			if !validFlag(flag.Name) || strings.ContainsAny(flag.Value, "\x00\n") {
				return nil, fmt.Errorf("technique %s: %q is not an engine flag", file.Manifest.ID, flag.Name)
			}
		}
		for _, name := range change.Remove {
			if !validFlag(name) {
				return nil, fmt.Errorf("technique %s: %q is not an engine flag", file.Manifest.ID, name)
			}
		}
		if change.Prediction != nil && change.Prediction.Metric != file.Manifest.Prediction.Metric {
			return nil, fmt.Errorf("technique %s predicts in %s", file.Manifest.ID, file.Manifest.Prediction.Metric)
		}
	}
	return static{manifest: file.Manifest, changes: file.Changes}, nil
}

func validFlag(name string) bool {
	return strings.HasPrefix(name, "--") && len(name) > 2 && len(name) <= 64 && !strings.ContainsAny(name, " =\x00\n")
}

func (s static) Manifest() Manifest { return s.manifest }

// Candidates applies each change to the starting setup's flags. A change the
// setup already has is skipped.
func (s static) Candidates(_ context.Context, setup Setup) ([]Candidate, string, error) {
	var out []Candidate
	for _, change := range s.changes {
		args, changed := ApplyFlags(setup.Args, change.Set, change.Remove)
		if !changed {
			continue
		}
		out = append(out, Candidate{Technique: s.manifest.ID, TechniqueVersion: s.manifest.Version, Label: change.Label, Args: args, Env: setup.Env,
			Prediction: change.Prediction, ChangesAnswers: s.manifest.ChangesAnswers, Reason: change.Reason})
	}
	if len(out) == 0 {
		return nil, "the starting setup already makes every change", nil
	}
	return out, "", nil
}

// ApplyFlags returns args with each flag in set given its value (replacing
// the setup's own, in place) and each flag in remove taken out, and whether
// anything changed. A flag's value is the next token unless it starts with
// "--", or the part after "=".
func ApplyFlags(args []string, set []Flag, remove []string) ([]string, bool) {
	type pair struct {
		name, value string
		inline, has bool
	}
	var pairs []pair
	for index := 0; index < len(args); index++ {
		name, value, inline := strings.Cut(args[index], "=")
		if !strings.HasPrefix(name, "--") {
			// A bare value with no flag before it is kept as it stands.
			pairs = append(pairs, pair{name: args[index]})
			continue
		}
		entry := pair{name: name, value: value, inline: inline, has: inline}
		if !inline && index+1 < len(args) && !strings.HasPrefix(args[index+1], "--") {
			entry.value, entry.has = args[index+1], true
			index++
		}
		pairs = append(pairs, entry)
	}
	changed := false
	kept := pairs[:0]
	for _, entry := range pairs {
		if slicesContains(remove, entry.name) {
			changed = true
			continue
		}
		kept = append(kept, entry)
	}
	pairs = kept
	for _, flag := range set {
		found := false
		for index := range pairs {
			if pairs[index].name != flag.Name {
				continue
			}
			found = true
			if pairs[index].value != flag.Value || pairs[index].has != (flag.Value != "") {
				pairs[index].value, pairs[index].has, changed = flag.Value, flag.Value != "", true
			}
		}
		if !found {
			pairs = append(pairs, pair{name: flag.Name, value: flag.Value, has: flag.Value != ""})
			changed = true
		}
	}
	out := make([]string, 0, len(args)+2*len(set))
	for _, entry := range pairs {
		switch {
		case entry.inline && entry.has:
			out = append(out, entry.name+"="+entry.value)
		case entry.has:
			out = append(out, entry.name, entry.value)
		default:
			out = append(out, entry.name)
		}
	}
	return out, changed
}

func slicesContains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

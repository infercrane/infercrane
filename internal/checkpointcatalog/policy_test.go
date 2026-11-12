package checkpointcatalog

import (
	"os"
	"strings"
	"testing"
)

func TestPolicySelectionRuleIsReviewedAndBounded(t *testing.T) {
	raw, err := os.ReadFile("policy.json")
	if err != nil {
		t.Fatal(err)
	}
	policy, _, err := DecodePolicy(raw)
	if err != nil {
		t.Fatal(err)
	}
	if policy.Selection.MinDownloads <= 0 || len(policy.Selection.Collections) == 0 {
		t.Fatalf("the committed policy lost its download threshold or collection seed: %+v", policy.Selection)
	}
	for _, publisher := range policy.Publishers {
		if publisher.CollectionOnly && publisher.ReviewState == ReviewApproved {
			t.Fatalf("collection-only publisher %s was approved without a recorded review", publisher.Organization)
		}
	}
	cases := map[string]func(*Policy){
		"missing note":        func(p *Policy) { p.Selection.Note = " " },
		"negative threshold":  func(p *Policy) { p.Selection.MinDownloads = -1 },
		"unsafe slug":         func(p *Policy) { p.Selection.Collections[0].Slug = "nvidia/../escape" },
		"slug without owner":  func(p *Policy) { p.Selection.Collections[0].Slug = "collection" },
		"unknown curator":     func(p *Policy) { p.Selection.Collections[0].Curator = "someone-else" },
		"collection note":     func(p *Policy) { p.Selection.Collections[0].Note = "" },
		"duplicate slug":      func(p *Policy) { p.Selection.Collections = append(p.Selection.Collections, p.Selection.Collections[0]) },
		"listed without date": func(p *Policy) { p.Publishers[0].CreatedAfter = "" },
		"collection-only with date": func(p *Policy) {
			p.Publishers[0].CollectionOnly = true
		},
		"collection-only without collections": func(p *Policy) {
			p.Publishers[0].CollectionOnly, p.Publishers[0].CreatedAfter = true, ""
			p.Selection.Collections = nil
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			copied, _, err := DecodePolicy(raw)
			if err != nil {
				t.Fatal(err)
			}
			mutate(&copied)
			if err := ValidatePolicy(copied); err == nil {
				t.Fatal("invalid selection rule was accepted")
			}
		})
	}
	if _, _, err := DecodePolicy([]byte(strings.Replace(string(raw), `"selection": {`, `"selection_rule": {`, 1))); err == nil {
		t.Fatal("a policy without the selection rule was accepted")
	}
}

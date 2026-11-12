package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtures = "../../internal/checkpointsync/testdata"

func TestFetchRequiresExplicitNetworkOptIn(t *testing.T) {
	err := run(context.Background(), []string{"fetch", "--out", t.TempDir(), "--policy", filepath.Join(fixtures, "policy.json"), "--no-previous"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "--network") {
		t.Fatalf("fetch ran without --network: %v", err)
	}
}

func TestPlanWritesProposalAndRequiresReviewDateToReplaceCatalog(t *testing.T) {
	out := t.TempDir()
	catalog := filepath.Join(out, "committed.json")
	previous, err := os.ReadFile(filepath.Join(fixtures, "previous.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(catalog, previous, 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"plan", "--snapshot", filepath.Join(fixtures, "snapshot"), "--policy", filepath.Join(fixtures, "policy.json"), "--catalog", catalog, "--out", out}
	if err = run(context.Background(), append(args, "--write"), &bytes.Buffer{}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "--reviewed-at") {
		t.Fatalf("catalog was replaced without a review date: %v", err)
	}
	var stdout bytes.Buffer
	if err = run(context.Background(), args, &stdout, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	golden, err := os.ReadFile(filepath.Join(fixtures, "golden", "catalog-diff.md"))
	if err != nil {
		t.Fatal(err)
	}
	diff, err := os.ReadFile(filepath.Join(out, "catalog-diff.md"))
	if err != nil || !bytes.Equal(diff, golden) {
		t.Fatalf("plan diff differs from the golden fixture: %v", err)
	}
	if unchanged, _ := os.ReadFile(catalog); !bytes.Equal(unchanged, previous) {
		t.Fatal("plan without --write changed the committed catalog")
	}
	if err = run(context.Background(), append(args, "--write", "--reviewed-at", "2026-11-11"), &stdout, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	written, _ := os.ReadFile(catalog)
	proposed, _ := os.ReadFile(filepath.Join(out, "catalog.json"))
	if !bytes.Equal(written, proposed) || !strings.Contains(stdout.String(), "wrote "+catalog) {
		t.Fatal("--write did not install the reviewed proposal")
	}
}

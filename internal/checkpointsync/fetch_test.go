package checkpointsync

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/checkpointcatalog"
)

var recordedStatus = regexp.MustCompile(`^\{"status":(\d+)\}\n$`)

// recordedUpstream serves the fixture snapshot as if it were the Hugging Face
// API, so the fetcher is tested without any live network access.
func recordedUpstream(t *testing.T, root string, throttleFirst bool) (*httptest.Server, *[]string) {
	t.Helper()
	var mutex sync.Mutex
	var requests []string
	throttled := !throttleFirst
	var server *httptest.Server
	serveFile := func(w http.ResponseWriter, path string) {
		raw, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			http.NotFound(w, nil)
			return
		}
		if match := recordedStatus.FindSubmatch(raw); match != nil {
			status := 0
			_ = json.Unmarshal(match[1], &status)
			w.WriteHeader(status)
			return
		}
		w.Header().Set("RateLimit", `"api";r=400;t=10`)
		_, _ = w.Write(raw)
	}
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mutex.Lock()
		requests = append(requests, r.URL.RequestURI())
		first := !throttled
		throttled = true
		mutex.Unlock()
		if r.Header.Get("User-Agent") != userAgent {
			t.Errorf("request without the generic client identity: %q", r.Header.Get("User-Agent"))
		}
		if first {
			w.Header().Set("RateLimit", `"api";r=0;t=3`)
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/")
		switch {
		case path == "api/models":
			organization, order := r.URL.Query().Get("author"), r.URL.Query().Get("sort")
			pagePath := listingPath
			if order == "downloads" {
				pagePath = downloadListingPath
			}
			page := 1
			if r.URL.Query().Get("cursor") == "2" {
				page = 2
			}
			if _, err := os.Stat(filepath.Join(root, pagePath(organization, page+1))); err == nil {
				w.Header().Set("Link", "<"+server.URL+"/api/models?author="+organization+"&sort="+order+"&cursor=2>; rel=\"next\"")
			}
			serveFile(w, pagePath(organization, page))
		case strings.HasPrefix(path, "api/collections/"):
			serveFile(w, collectionPath(strings.TrimPrefix(path, "api/collections/")))
		case strings.HasPrefix(path, "api/models/") && strings.Contains(path, "/revision/"):
			repository, revision, _ := strings.Cut(strings.TrimPrefix(path, "api/models/"), "/revision/")
			serveFile(w, infoPath(repository, revision))
		case strings.HasPrefix(path, "api/models/"):
			repository := strings.TrimPrefix(path, "api/models/")
			if _, err := os.Stat(filepath.Join(root, headPath(repository))); err == nil {
				serveFile(w, headPath(repository))
				return
			}
			serveFile(w, basePath(repository))
		case strings.Contains(path, "/raw/"):
			repository, rest, _ := strings.Cut(path, "/raw/")
			revision, name, _ := strings.Cut(rest, "/")
			serveFile(w, filePath(repository, revision, name))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

func TestFetchRecordsTheSnapshotThePlanReads(t *testing.T) {
	root := filepath.Join("testdata", "snapshot")
	server, requests := recordedUpstream(t, root, true)
	policy, digest := fixturePolicy(t)
	var sleeps []time.Duration
	fetcher := Fetcher{
		Endpoint: server.URL, Policy: policy, PolicyDigest: digest, Previous: fixturePrevious(t),
		Now:   func() time.Time { return time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC) },
		Sleep: func(d time.Duration) { sleeps = append(sleeps, d) },
	}
	out := t.TempDir()
	manifest, err := fetcher.Fetch(context.Background(), out)
	if err != nil {
		t.Fatal(err)
	}
	if len(sleeps) != 1 || sleeps[0] != 4*time.Second {
		t.Fatalf("rate limit was not honored: %v", sleeps)
	}
	recorded, err := OpenSnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.FetchedAt != recorded.Manifest.FetchedAt || manifest.PolicyDigest != recorded.Manifest.PolicyDigest || strings.Join(manifest.Pins, ",") != strings.Join(recorded.Manifest.Pins, ",") || len(manifest.Publishers) != len(recorded.Manifest.Publishers) {
		t.Fatalf("manifest mismatch: got %+v want %+v", manifest, recorded.Manifest)
	}
	for index, publisher := range manifest.Publishers {
		if publisher != recorded.Manifest.Publishers[index] {
			t.Fatalf("publisher window mismatch: got %+v want %+v", publisher, recorded.Manifest.Publishers[index])
		}
	}
	want := map[string][]byte{}
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && entry.Name() != "snapshot.json" {
			relative, _ := filepath.Rel(root, path)
			want[relative], _ = os.ReadFile(path)
		}
		return err
	})
	got := map[string][]byte{}
	_ = filepath.WalkDir(out, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && entry.Name() != "snapshot.json" {
			relative, _ := filepath.Rel(out, path)
			got[relative], _ = os.ReadFile(path)
		}
		return err
	})
	for path, raw := range want {
		if !bytes.Equal(got[path], raw) {
			t.Errorf("fetched %s differs from the recorded fixture", path)
		}
	}
	for path := range got {
		if _, expected := want[path]; !expected {
			t.Errorf("fetch recorded an unexpected file %s", path)
		}
	}
	for _, request := range *requests {
		if strings.Contains(request, "Old-Model") || strings.Contains(request, "Cosmos3") {
			t.Fatalf("fetch requested details for a screened-out repository: %s", request)
		}
	}
	fetched, err := OpenSnapshot(out)
	if err != nil {
		t.Fatal(err)
	}
	// The test server is plain HTTP; catalogs only accept HTTPS sources.
	fetched.Manifest.Source = recorded.Manifest.Source
	fromFetch, err := Plan(fetched, policy, digest, fixturePrevious(t), "2026-11-11")
	if err != nil {
		t.Fatal(err)
	}
	fromFixture := fixturePlan(t, policy, digest, fixturePrevious(t))
	left, _ := json.Marshal(fromFetch.Catalog.Entries)
	right, _ := json.Marshal(fromFixture.Catalog.Entries)
	if !bytes.Equal(left, right) {
		t.Fatal("planning the fetched snapshot differs from planning the recorded fixture")
	}
}

func TestFetchRefusesForeignPaginationAndExistingSnapshots(t *testing.T) {
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/collections/") {
			_, _ = w.Write([]byte(`{"items":[]}`))
			return
		}
		w.Header().Set("Link", `<https://example.invalid/api/models?cursor=2>; rel="next"`)
		_, _ = w.Write([]byte(`[{"id":"nvidia/A","sha":"` + strings.Repeat("a", 40) + `","createdAt":"2026-10-01T00:00:00.000Z"}]`))
	}))
	t.Cleanup(foreign.Close)
	policy, digest := fixturePolicy(t)
	fetcher := Fetcher{Endpoint: foreign.URL, Policy: policy, PolicyDigest: digest, Sleep: func(time.Duration) {}}
	if _, err := fetcher.Fetch(context.Background(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "refusing pagination link") {
		t.Fatalf("foreign pagination link was followed: %v", err)
	}
	existing := t.TempDir()
	if err := os.WriteFile(filepath.Join(existing, "snapshot.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := fetcher.Fetch(context.Background(), existing); err == nil {
		t.Fatal("fetch overwrote an existing snapshot")
	}
	bad := Fetcher{Policy: policy, Pins: []string{"nvidia/A@main"}, Sleep: func(time.Duration) {}}
	if _, err := bad.Fetch(context.Background(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("mutable pin was accepted: %v", err)
	}
}

func TestPlanRefusesIncompleteSnapshots(t *testing.T) {
	if _, err := OpenSnapshot(t.TempDir()); err == nil {
		t.Fatal("a snapshot without a manifest was accepted")
	}
	dir := t.TempDir()
	raw, _ := os.ReadFile(filepath.Join("testdata", "snapshot", "snapshot.json"))
	if err := os.WriteFile(filepath.Join(dir, "snapshot.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot, err := OpenSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	policy, digest := fixturePolicy(t)
	if _, err = Plan(snapshot, policy, digest, fixturePrevious(t), "2026-11-11"); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("plan accepted a snapshot without recorded revisions: %v", err)
	}
}

func TestFetchSeedsReviewedCollectionsAndListsByDownloads(t *testing.T) {
	root := filepath.Join("testdata", "snapshot")
	server, requests := recordedUpstream(t, root, false)
	policy, digest := fixturePolicy(t)
	fetcher := Fetcher{Endpoint: server.URL, Policy: policy, PolicyDigest: digest, Previous: fixturePrevious(t), Sleep: func(time.Duration) {}}
	manifest, err := fetcher.Fetch(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Collections) != 1 || manifest.Collections[0].Members != 6 {
		t.Fatalf("collection members were not recorded: %+v", manifest.Collections)
	}
	counts := map[string]int{}
	for _, request := range *requests {
		switch {
		case strings.HasPrefix(request, "/api/collections/nvidia/test-modelopt-checkpoints-0123456789abcdef"):
			counts["collection"]++
		case strings.Contains(request, "sort=downloads"):
			counts["downloads"]++
		case strings.HasPrefix(request, "/api/models/nvidia/Hidden-Signal-8B-FP8?"):
			t.Fatalf("a listed collection member was fetched again as a head record: %s", request)
		case strings.Contains(request, "author=stepfun-ai"):
			t.Fatalf("a collection-only publisher was listed as an organization: %s", request)
		case strings.Contains(request, "black-forest-labs") || strings.Contains(request, "Test-Dataset") || strings.Contains(request, "escape"):
			t.Fatalf("fetch followed a collection item outside the policy: %s", request)
		case strings.HasPrefix(request, "/api/models/nvidia/Old-Listed-NVFP4?"), strings.HasPrefix(request, "/api/models/stepfun-ai/Step-Test-NVFP4?"):
			counts["member head"]++
		}
	}
	if counts["collection"] != 1 || counts["downloads"] != 3 || counts["member head"] != 2 {
		t.Fatalf("unexpected seed requests: %+v", counts)
	}

	policy.Selection.MinDownloads = 0
	policy.Selection.Collections = nil
	policy.Publishers = slices.DeleteFunc(policy.Publishers, func(publisher checkpointcatalog.PublisherPolicy) bool { return publisher.CollectionOnly })
	*requests = nil
	manifest, err = (&Fetcher{Endpoint: server.URL, Policy: policy, Sleep: func(time.Duration) {}}).Fetch(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range *requests {
		if strings.Contains(request, "sort=downloads") || strings.Contains(request, "/api/collections/") {
			t.Fatalf("a disabled selection rule still sent %s", request)
		}
	}
	if len(manifest.Collections) != 0 || manifest.Publishers[0].MinDownloads != 0 {
		t.Fatalf("manifest records rules the policy did not set: %+v", manifest)
	}
}

func TestFetchStopsWhenAReviewedCollectionIsMissing(t *testing.T) {
	missing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/collections/") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(missing.Close)
	policy, digest := fixturePolicy(t)
	fetcher := Fetcher{Endpoint: missing.URL, Policy: policy, PolicyDigest: digest, Sleep: func(time.Duration) {}}
	if _, err := fetcher.Fetch(context.Background(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "review the policy") {
		t.Fatalf("a missing reviewed collection did not stop the fetch: %v", err)
	}
}

func TestSnapshotDigestIgnoresPlanOutput(t *testing.T) {
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(filepath.Join("testdata", "snapshot"))); err != nil {
		t.Fatal(err)
	}
	snapshot, err := OpenSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	before, err := snapshot.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if err = writeFile(dir, filepath.Join(PlanDir, "catalog.json"), []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	if after, _ := snapshot.Digest(); after != before {
		t.Fatal("writing plan output changed the snapshot digest")
	}
	if err = writeFile(dir, filepath.Join("listings", "nvidia", "page-0002.json"), []byte("[]\n")); err != nil {
		t.Fatal(err)
	}
	if after, _ := snapshot.Digest(); after == before {
		t.Fatal("a recorded upstream file did not change the snapshot digest")
	}
}

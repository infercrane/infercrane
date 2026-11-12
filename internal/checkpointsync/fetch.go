package checkpointsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/checkpointcatalog"
)

const (
	DefaultEndpoint = "https://huggingface.co"
	userAgent       = "infercrane-checkpoint-catalog-sync"
	maxListingPages = 100
	maxFileBytes    = 8 << 20
	maxRecordBytes  = 64 << 20
)

var (
	listingFields = []string{"sha", "createdAt", "lastModified", "private", "disabled", "gated", "library_name", "pipeline_tag", "tags", "cardData", "config"}
	baseFields    = []string{"sha", "lastModified", "private", "disabled", "gated", "cardData"}
	nextLink      = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="next"`)
	rateLimit     = regexp.MustCompile(`r=(\d+)\s*;\s*t=(\d+)`)
)

// Fetcher records one upstream observation. It is the only network client in
// the checkpoint catalog flow and runs only when a caller asks for it.
type Fetcher struct {
	Endpoint     string
	Client       *http.Client
	Policy       checkpointcatalog.Policy
	PolicyDigest string
	Previous     checkpointcatalog.Catalog
	Pins         []string
	Now          func() time.Time
	Sleep        func(time.Duration)
	Log          io.Writer
}

type pin struct {
	repository string
	revision   string
}

func (f *Fetcher) Fetch(ctx context.Context, dir string) (Manifest, error) {
	if f.Endpoint == "" {
		f.Endpoint = DefaultEndpoint
	}
	endpoint, err := url.Parse(f.Endpoint)
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" {
		return Manifest{}, errors.New("fetch endpoint must be an absolute URL")
	}
	if f.Client == nil {
		f.Client = &http.Client{Timeout: 90 * time.Second}
	}
	client := *f.Client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	f.Client = &client
	if f.Now == nil {
		f.Now = time.Now
	}
	if f.Sleep == nil {
		f.Sleep = time.Sleep
	}
	if f.Log == nil {
		f.Log = io.Discard
	}
	if _, statErr := os.Stat(filepath.Join(dir, "snapshot.json")); statErr == nil {
		return Manifest{}, fmt.Errorf("%s already holds a snapshot", dir)
	}
	pins, err := f.pins()
	if err != nil {
		return Manifest{}, err
	}
	manifest := Manifest{SchemaVersion: SnapshotSchemaVersion, Source: strings.TrimRight(f.Endpoint, "/"), FetchedAt: f.Now().UTC().Format(time.RFC3339), PolicyDigest: f.PolicyDigest}
	heads := map[string]string{}
	publishers := append([]checkpointcatalog.PublisherPolicy(nil), f.Policy.Publishers...)
	sort.Slice(publishers, func(i, j int) bool { return publishers[i].Organization < publishers[j].Organization })
	for _, publisher := range publishers {
		items, pages, listErr := f.list(ctx, dir, publisher)
		if listErr != nil {
			return Manifest{}, listErr
		}
		manifest.Publishers = append(manifest.Publishers, ManifestPublisher{Organization: publisher.Organization, CreatedAfter: publisher.CreatedAfter, Pages: pages})
		kept := 0
		for _, item := range items {
			if inWindow(item, publisher) && screen(item) == "" {
				if _, excluded := f.Policy.Excluded(item.ID, ""); !excluded {
					heads[item.ID] = item.SHA
					kept++
				}
			}
		}
		fmt.Fprintf(f.Log, "%s: %d listed, %d kept for detail\n", publisher.Organization, len(items), kept)
	}
	targets := map[pin]struct{}{}
	for repository, revision := range heads {
		targets[pin{repository, revision}] = struct{}{}
	}
	for _, pinned := range pins {
		manifest.Pins = append(manifest.Pins, pinned.repository+"@"+pinned.revision)
		targets[pinned] = struct{}{}
		if _, listed := heads[pinned.repository]; listed {
			continue
		}
		head, found, headErr := f.record(ctx, dir, headPath(pinned.repository), f.modelURL(pinned.repository, listingFields))
		if headErr != nil {
			return Manifest{}, headErr
		}
		if found && head.Status == 0 && screen(head) == "" {
			heads[pinned.repository] = head.SHA
			targets[pin{pinned.repository, head.SHA}] = struct{}{}
		}
	}
	ordered := make([]pin, 0, len(targets))
	for target := range targets {
		ordered = append(ordered, target)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].repository != ordered[j].repository {
			return ordered[i].repository < ordered[j].repository
		}
		return ordered[i].revision < ordered[j].revision
	})
	bases := map[string]struct{}{}
	for index, target := range ordered {
		info, found, detailErr := f.record(ctx, dir, infoPath(target.repository, target.revision), strings.TrimRight(f.Endpoint, "/")+"/api/models/"+target.repository+"/revision/"+target.revision+"?blobs=true")
		if detailErr != nil {
			return Manifest{}, detailErr
		}
		if !found || info.Status != 0 {
			continue
		}
		for _, name := range recordedFiles(info) {
			raw, status, fileErr := f.get(ctx, strings.TrimRight(f.Endpoint, "/")+"/"+target.repository+"/raw/"+target.revision+"/"+url.PathEscape(name), maxFileBytes)
			if fileErr != nil {
				return Manifest{}, fileErr
			}
			if status == http.StatusOK {
				if writeErr := writeFile(dir, filePath(target.repository, target.revision, name), raw); writeErr != nil {
					return Manifest{}, writeErr
				}
			}
		}
		for _, base := range baseModels(info) {
			if ValidRepository(base) {
				bases[base] = struct{}{}
			}
		}
		if (index+1)%25 == 0 {
			fmt.Fprintf(f.Log, "details: %d/%d\n", index+1, len(ordered))
		}
	}
	baseList := make([]string, 0, len(bases))
	for base := range bases {
		baseList = append(baseList, base)
	}
	sort.Strings(baseList)
	for _, base := range baseList {
		if _, _, baseErr := f.record(ctx, dir, basePath(base), f.modelURL(base, baseFields)); baseErr != nil {
			return Manifest{}, baseErr
		}
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return Manifest{}, err
	}
	if err = writeFile(dir, "snapshot.json", append(raw, '\n')); err != nil {
		return Manifest{}, err
	}
	fmt.Fprintf(f.Log, "snapshot: %d revisions, %d base models\n", len(ordered), len(baseList))
	return manifest, nil
}

func (f *Fetcher) pins() ([]pin, error) {
	set := map[pin]struct{}{}
	for _, entry := range f.Previous.Entries {
		set[pin{entry.Repository, entry.Revision}] = struct{}{}
	}
	for _, value := range f.Pins {
		repository, revision, found := strings.Cut(value, "@")
		if !found || !ValidRepository(repository) || !checkpointcatalog.ImmutableRevision(revision) {
			return nil, fmt.Errorf("pin %q must be repository@immutable-revision", value)
		}
		set[pin{repository, strings.ToLower(revision)}] = struct{}{}
	}
	out := make([]pin, 0, len(set))
	for value := range set {
		if !ValidRepository(value.repository) {
			return nil, fmt.Errorf("catalog repository %q is not a valid identifier", value.repository)
		}
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].repository+"@"+out[i].revision < out[j].repository+"@"+out[j].revision
	})
	return out, nil
}

func inWindow(info ModelInfo, publisher checkpointcatalog.PublisherPolicy) bool {
	return len(info.CreatedAt) >= 10 && info.CreatedAt[:10] >= publisher.CreatedAfter
}

// list pages through one organization, newest first, until it passes the
// publisher's created_after boundary. Org listings are used instead of
// collections because collections lag the organization.
func (f *Fetcher) list(ctx context.Context, dir string, publisher checkpointcatalog.PublisherPolicy) ([]ModelInfo, int, error) {
	query := url.Values{"author": {publisher.Organization}, "sort": {"createdAt"}, "direction": {"-1"}, "limit": {"100"}, "expand": listingFields}
	next := strings.TrimRight(f.Endpoint, "/") + "/api/models?" + query.Encode()
	var items []ModelInfo
	pages := 0
	for next != "" && pages < maxListingPages {
		raw, header, status, err := f.request(ctx, next, maxRecordBytes)
		if err != nil {
			return nil, 0, err
		}
		if status != http.StatusOK {
			return nil, 0, fmt.Errorf("list %s: upstream status %d", publisher.Organization, status)
		}
		var page []ModelInfo
		if err = json.Unmarshal(raw, &page); err != nil {
			return nil, 0, fmt.Errorf("decode %s listing: %w", publisher.Organization, err)
		}
		pages++
		if err = writeFile(dir, listingPath(publisher.Organization, pages), raw); err != nil {
			return nil, 0, err
		}
		items = append(items, page...)
		if len(page) == 0 || !inWindow(page[len(page)-1], publisher) {
			break
		}
		next = ""
		if match := nextLink.FindStringSubmatch(header.Get("Link")); match != nil {
			link, parseErr := url.Parse(match[1])
			base, _ := url.Parse(f.Endpoint)
			if parseErr != nil || link.Host != base.Host || link.Scheme != base.Scheme {
				return nil, 0, fmt.Errorf("list %s: refusing pagination link outside %s", publisher.Organization, f.Endpoint)
			}
			next = link.String()
		}
	}
	return items, pages, nil
}

func (f *Fetcher) modelURL(repository string, fields []string) string {
	return strings.TrimRight(f.Endpoint, "/") + "/api/models/" + repository + "?" + url.Values{"expand": fields}.Encode()
}

// record fetches one model record and writes it, recording a missing or
// forbidden repository as {"status": N} instead of failing the run.
func (f *Fetcher) record(ctx context.Context, dir, path, target string) (ModelInfo, bool, error) {
	raw, status, err := f.get(ctx, target, maxRecordBytes)
	if err != nil {
		return ModelInfo{}, false, err
	}
	if status != http.StatusOK {
		raw = []byte(fmt.Sprintf("{\"status\":%d}\n", status))
	}
	if err = writeFile(dir, path, raw); err != nil {
		return ModelInfo{}, false, err
	}
	var info ModelInfo
	if err = json.Unmarshal(raw, &info); err != nil {
		return ModelInfo{}, false, fmt.Errorf("decode %s: %w", target, err)
	}
	return info, true, nil
}

func (f *Fetcher) get(ctx context.Context, target string, limit int64) ([]byte, int, error) {
	raw, _, status, err := f.request(ctx, target, limit)
	return raw, status, err
}

// request performs one GET with bounded retries. 401, 403, 404, 410,
// redirects, and oversized bodies (413) are returned as statuses; other
// failures stop the run.
func (f *Fetcher) request(ctx context.Context, target string, limit int64) ([]byte, http.Header, int, error) {
	for attempt := 0; ; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, nil, 0, err
		}
		request.Header.Set("User-Agent", userAgent)
		request.Header.Set("Accept", "application/json, text/plain, */*")
		response, err := f.Client.Do(request)
		if err != nil {
			if attempt < 3 && ctx.Err() == nil {
				f.Sleep(time.Duration(1<<attempt) * time.Second)
				continue
			}
			return nil, nil, 0, fmt.Errorf("GET %s: %w", target, err)
		}
		raw, readErr := io.ReadAll(io.LimitReader(response.Body, limit+1))
		response.Body.Close()
		status := response.StatusCode
		switch {
		case status == http.StatusTooManyRequests && attempt < 6:
			f.Sleep(retryDelay(response.Header, 60*time.Second))
			continue
		case status >= 500 && attempt < 3:
			f.Sleep(time.Duration(1<<attempt) * time.Second)
			continue
		case status == http.StatusOK:
			if readErr != nil {
				return nil, nil, 0, fmt.Errorf("GET %s: %w", target, readErr)
			}
			if int64(len(raw)) > limit {
				fmt.Fprintf(f.Log, "skipping %s: response exceeds %d bytes\n", target, limit)
				f.respectRateLimit(response.Header)
				return nil, response.Header, http.StatusRequestEntityTooLarge, nil
			}
			f.respectRateLimit(response.Header)
			return raw, response.Header, status, nil
		case status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusNotFound || status == http.StatusGone || status >= 300 && status < 400:
			f.respectRateLimit(response.Header)
			return nil, response.Header, status, nil
		}
		return nil, nil, 0, fmt.Errorf("GET %s: upstream status %d", target, status)
	}
}

func (f *Fetcher) respectRateLimit(header http.Header) {
	match := rateLimit.FindStringSubmatch(header.Get("RateLimit"))
	if match == nil {
		return
	}
	remaining, _ := strconv.Atoi(match[1])
	reset, _ := strconv.Atoi(match[2])
	if remaining <= 2 {
		fmt.Fprintf(f.Log, "rate limit reached; waiting %ds\n", reset+1)
		f.Sleep(time.Duration(reset+1) * time.Second)
	}
}

func retryDelay(header http.Header, fallback time.Duration) time.Duration {
	if match := rateLimit.FindStringSubmatch(header.Get("RateLimit")); match != nil {
		if reset, err := strconv.Atoi(match[2]); err == nil {
			return time.Duration(reset+1) * time.Second
		}
	}
	if seconds, err := strconv.Atoi(header.Get("Retry-After")); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return fallback
}

func writeFile(dir, path string, raw []byte) error {
	target := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	return os.WriteFile(target, raw, 0o644)
}

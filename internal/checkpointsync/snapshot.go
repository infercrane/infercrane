// Package checkpointsync refreshes the reviewed checkpoint catalog. Fetch is
// the only code that touches the network: it records raw Hugging Face API
// responses into a snapshot directory. Plan reads only that snapshot, the
// reviewed policy, and the committed catalog, and produces a deterministic
// proposed catalog plus a reviewable diff.
package checkpointsync

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const SnapshotSchemaVersion = "infercrane.checkpoint-snapshot/v1"

// Manifest is written last by Fetch, so a snapshot without it is incomplete.
type Manifest struct {
	SchemaVersion string               `json:"schema_version"`
	Source        string               `json:"source"`
	FetchedAt     string               `json:"fetched_at"`
	PolicyDigest  string               `json:"policy_digest"`
	Publishers    []ManifestPublisher  `json:"publishers"`
	Collections   []ManifestCollection `json:"collections,omitempty"`
	Pins          []string             `json:"pins"`
}

// ManifestPublisher records the selection window a publisher was fetched with,
// so a plan applies the rule the snapshot was recorded under.
type ManifestPublisher struct {
	Organization   string `json:"organization"`
	CreatedAfter   string `json:"created_after,omitempty"`
	MinDownloads   int64  `json:"min_downloads,omitempty"`
	CollectionOnly bool   `json:"collection_only,omitempty"`
	Pages          int    `json:"pages"`
	DownloadPages  int    `json:"download_pages,omitempty"`
}

// ManifestCollection records one reviewed collection and how many model
// members it listed when the snapshot was fetched.
type ManifestCollection struct {
	Slug    string `json:"slug"`
	Members int    `json:"members"`
}

// selects applies the reviewed selection rule to one organization listing
// item. It returns why the item is tracked, or false when it is not.
func (window ManifestPublisher) selects(item ModelInfo) (string, bool) {
	if window.CollectionOnly {
		return "", false
	}
	if window.CreatedAfter != "" && len(item.CreatedAt) >= 10 && item.CreatedAt[:10] >= window.CreatedAfter {
		return fmt.Sprintf("created %s (on or after %s)", item.CreatedAt[:10], window.CreatedAfter), true
	}
	if window.MinDownloads > 0 && item.Downloads >= window.MinDownloads {
		return fmt.Sprintf("%d downloads in 30 days (at least %d)", item.Downloads, window.MinDownloads), true
	}
	return "", false
}

// Snapshot is a recorded upstream observation on disk:
//
//	snapshot.json
//	listings/<org>/page-0001.json          raw org listing pages, newest first
//	listings/<org>/downloads-0001.json     raw org listing pages, most downloaded first
//	collections/<owner>/<slug>.json        raw reviewed collection
//	heads/<org>/<name>.json                head record for pinned repositories
//	models/<org>/<name>/<sha>/info.json    revision record with file blobs
//	models/<org>/<name>/<sha>/files/<f>    README.md, config.json, licenses
//	bases/<org>/<name>.json                base model head record
type Snapshot struct {
	Dir      string
	Manifest Manifest
}

type fileReader func(name string) ([]byte, bool)

func OpenSnapshot(dir string) (*Snapshot, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "snapshot.json"))
	if err != nil {
		return nil, fmt.Errorf("snapshot is incomplete or missing: %w", err)
	}
	var manifest Manifest
	if err = json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("decode snapshot manifest: %w", err)
	}
	if manifest.SchemaVersion != SnapshotSchemaVersion || !strings.HasPrefix(manifest.Source, "http") || manifest.FetchedAt == "" {
		return nil, errors.New("snapshot manifest is unsupported or incomplete")
	}
	return &Snapshot{Dir: dir, Manifest: manifest}, nil
}

// Listing returns every recorded newest-first listing item for one
// organization.
func (snapshot *Snapshot) Listing(organization string) ([]ModelInfo, error) {
	return snapshot.listing(organization, "page-*.json")
}

// DownloadListing returns every recorded most-downloaded-first listing item
// for one organization.
func (snapshot *Snapshot) DownloadListing(organization string) ([]ModelInfo, error) {
	return snapshot.listing(organization, "downloads-*.json")
}

// CollectionMembers returns the model repositories a recorded collection
// lists, in collection order.
func (snapshot *Snapshot) CollectionMembers(slug string) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join(snapshot.Dir, collectionPath(slug)))
	if err != nil {
		return nil, fmt.Errorf("snapshot is incomplete: collection %s: %w", slug, err)
	}
	return collectionMembers(raw)
}

func (snapshot *Snapshot) listing(organization, pattern string) ([]ModelInfo, error) {
	pages, err := filepath.Glob(filepath.Join(snapshot.Dir, "listings", organization, pattern))
	if err != nil {
		return nil, err
	}
	sort.Strings(pages)
	var out []ModelInfo
	for _, page := range pages {
		raw, readErr := os.ReadFile(page)
		if readErr != nil {
			return nil, readErr
		}
		var items []ModelInfo
		if readErr = json.Unmarshal(raw, &items); readErr != nil {
			return nil, fmt.Errorf("decode %s: %w", page, readErr)
		}
		out = append(out, items...)
	}
	return out, nil
}

func (snapshot *Snapshot) record(path string) (ModelInfo, bool, error) {
	raw, err := os.ReadFile(filepath.Join(snapshot.Dir, path))
	if errors.Is(err, fs.ErrNotExist) {
		return ModelInfo{}, false, nil
	}
	if err != nil {
		return ModelInfo{}, false, err
	}
	var info ModelInfo
	if err = json.Unmarshal(raw, &info); err != nil {
		return ModelInfo{}, false, fmt.Errorf("decode %s: %w", path, err)
	}
	return info, true, nil
}

func (snapshot *Snapshot) Head(repository string) (ModelInfo, bool, error) {
	return snapshot.record(headPath(repository))
}

func (snapshot *Snapshot) Info(repository, revision string) (ModelInfo, bool, error) {
	return snapshot.record(infoPath(repository, revision))
}

func (snapshot *Snapshot) Base(repository string) (ModelInfo, bool, error) {
	return snapshot.record(basePath(repository))
}

func (snapshot *Snapshot) Files(repository, revision string) fileReader {
	return func(name string) ([]byte, bool) {
		raw, err := os.ReadFile(filepath.Join(snapshot.Dir, filePath(repository, revision, name)))
		return raw, err == nil
	}
}

// Digest hashes every recorded file by path and content, so a catalog can name
// exactly which observation produced it.
func (snapshot *Snapshot) Digest() (string, error) {
	var paths []string
	err := filepath.WalkDir(snapshot.Dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		relative, relErr := filepath.Rel(snapshot.Dir, path)
		if relErr != nil {
			return relErr
		}
		paths = append(paths, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(paths)
	hash := sha256.New()
	for _, path := range paths {
		raw, readErr := os.ReadFile(filepath.Join(snapshot.Dir, filepath.FromSlash(path)))
		if readErr != nil {
			return "", readErr
		}
		sum := sha256.Sum256(raw)
		fmt.Fprintf(hash, "%s\t%s\n", path, hex.EncodeToString(sum[:]))
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func headPath(repository string) string {
	return filepath.Join("heads", filepath.FromSlash(repository)+".json")
}

func basePath(repository string) string {
	return filepath.Join("bases", filepath.FromSlash(repository)+".json")
}

func infoPath(repository, revision string) string {
	return filepath.Join("models", filepath.FromSlash(repository), revision, "info.json")
}

func filePath(repository, revision, name string) string {
	return filepath.Join("models", filepath.FromSlash(repository), revision, "files", name)
}

func listingPath(organization string, page int) string {
	return filepath.Join("listings", organization, fmt.Sprintf("page-%04d.json", page))
}

func downloadListingPath(organization string, page int) string {
	return filepath.Join("listings", organization, fmt.Sprintf("downloads-%04d.json", page))
}

func collectionPath(slug string) string {
	return filepath.Join("collections", filepath.FromSlash(slug)+".json")
}

// collectionMembers decodes a Hugging Face collection and returns the model
// repositories it lists. Datasets, Spaces, and papers are ignored, and so is
// an identifier that is not safe to use in a snapshot path.
func collectionMembers(raw []byte) ([]string, error) {
	var collection struct {
		Items []struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &collection); err != nil {
		return nil, fmt.Errorf("decode collection: %w", err)
	}
	var members []string
	seen := map[string]struct{}{}
	for _, item := range collection.Items {
		if item.Type != "model" || !ValidRepository(item.ID) {
			continue
		}
		if _, duplicate := seen[item.ID]; !duplicate {
			seen[item.ID] = struct{}{}
			members = append(members, item.ID)
		}
	}
	return members, nil
}

// recordedFiles are the small files the plan reads at each revision.
func recordedFiles(info ModelInfo) []string {
	var names []string
	for _, name := range []string{"README.md", "config.json", "hf_quant_config.json"} {
		if info.hasFile(name) {
			names = append(names, name)
		}
	}
	if license := info.licenseFile(); license != "" {
		names = append(names, license)
	}
	return names
}

// manifestDigest identifies the exact file set at a revision. It hashes each
// file's path, size, and content identity (LFS SHA-256 or Git blob ID).
func manifestDigest(info ModelInfo) (string, int, int64) {
	siblings := append([]Sibling(nil), info.Siblings...)
	sort.Slice(siblings, func(i, j int) bool { return siblings[i].Name < siblings[j].Name })
	hash := sha256.New()
	fmt.Fprint(hash, "infercrane.hf-siblings/v1\n")
	var size int64
	for _, sibling := range siblings {
		identity, bytes := "git:"+sibling.BlobID, sibling.Size
		if sibling.LFS != nil {
			identity, bytes = "lfs:"+sibling.LFS.SHA256, sibling.LFS.Size
		}
		fmt.Fprintf(hash, "%s\t%d\t%s\n", sibling.Name, bytes, identity)
		size += bytes
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), len(siblings), size
}

func digestBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

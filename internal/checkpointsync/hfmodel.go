package checkpointsync

import (
	"encoding/json"
	"regexp"
	"strings"
)

// ModelInfo is the subset of a Hugging Face model record the sync reads. The
// snapshot keeps the raw responses; this type only decodes them.
type ModelInfo struct {
	ID           string      `json:"id"`
	SHA          string      `json:"sha"`
	CreatedAt    string      `json:"createdAt"`
	LastModified string      `json:"lastModified"`
	Downloads    int64       `json:"downloads"`
	Private      bool        `json:"private"`
	Disabled     bool        `json:"disabled"`
	Gated        any         `json:"gated"`
	LibraryName  string      `json:"library_name"`
	PipelineTag  string      `json:"pipeline_tag"`
	Tags         []string    `json:"tags"`
	CardData     CardData    `json:"cardData"`
	Config       ModelConfig `json:"config"`
	Siblings     []Sibling   `json:"siblings"`
	// Status is written by the fetcher when the upstream answered with an
	// error status, so a missing repository is recorded rather than guessed.
	Status int `json:"status,omitempty"`
}

type CardData struct {
	BaseModel   stringList `json:"base_model"`
	License     stringList `json:"license"`
	LibraryName string     `json:"library_name"`
}

type ModelConfig struct {
	Architectures      []string       `json:"architectures"`
	ModelType          string         `json:"model_type"`
	QuantizationConfig map[string]any `json:"quantization_config"`
}

type Sibling struct {
	Name   string `json:"rfilename"`
	BlobID string `json:"blobId,omitempty"`
	Size   int64  `json:"size,omitempty"`
	LFS    *struct {
		SHA256 string `json:"sha256"`
		Size   int64  `json:"size"`
	} `json:"lfs,omitempty"`
}

// stringList accepts the string-or-list shapes model cards use.
type stringList []string

func (list *stringList) UnmarshalJSON(data []byte) error {
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		if strings.TrimSpace(single) != "" {
			*list = stringList{strings.TrimSpace(single)}
		}
		return nil
	}
	var many []any
	if err := json.Unmarshal(data, &many); err != nil {
		return nil
	}
	for _, value := range many {
		if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
			*list = append(*list, strings.TrimSpace(text))
		}
	}
	return nil
}

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}/[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)

// ValidRepository reports whether a repository ID is safe to use in snapshot
// paths and URLs.
func ValidRepository(repository string) bool {
	if !repositoryPattern.MatchString(repository) {
		return false
	}
	for _, part := range strings.Split(repository, "/") {
		if part == "." || part == ".." || strings.Contains(part, "..") {
			return false
		}
	}
	return true
}

func (info ModelInfo) organization() string {
	organization, _, _ := strings.Cut(info.ID, "/")
	return organization
}

func (info ModelInfo) name() string {
	_, name, _ := strings.Cut(info.ID, "/")
	return name
}

func (info ModelInfo) gated() bool {
	switch value := info.Gated.(type) {
	case bool:
		return value
	case string:
		return value != "" && value != "false"
	}
	return false
}

func (info ModelInfo) hasFile(name string) bool {
	for _, sibling := range info.Siblings {
		if sibling.Name == name {
			return true
		}
	}
	return false
}

func (info ModelInfo) hasTag(tag string) bool {
	for _, candidate := range info.Tags {
		if strings.EqualFold(candidate, tag) {
			return true
		}
	}
	return false
}

func (info ModelInfo) license() string {
	if len(info.CardData.License) > 0 {
		return strings.ToLower(info.CardData.License[0])
	}
	for _, tag := range info.Tags {
		if value, found := strings.CutPrefix(tag, "license:"); found {
			return strings.ToLower(value)
		}
	}
	return "unknown"
}

// licenseFile returns the first license file at the revision, if any.
func (info ModelInfo) licenseFile() string {
	for _, name := range []string{"LICENSE", "LICENSE.md", "LICENSE.txt", "LICENCE"} {
		if info.hasFile(name) {
			return name
		}
	}
	return ""
}

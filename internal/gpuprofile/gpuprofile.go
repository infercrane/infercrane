// Package gpuprofile defines the bounded, content-free GPU profile a collector
// records against a deployment's active revision: sampled GPU time attributed
// to call stacks that run from Python down to CUDA kernels, SASS instructions,
// and warp stall reasons.
package gpuprofile

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// SchemaVersion is the only accepted recording and response schema.
const SchemaVersion = "infercrane.gpu-profile/v1"

// Bounds on one recording. The dashboard enforces the same limits.
const (
	MaxRecordingBytes = 8 << 20
	MaxFrames         = 20_000
	MaxStacks         = 10_000
	MaxStackDepth     = 128
	MaxBuckets        = 500
	MaxFrameName      = 512
	MaxFrameFile      = 512
	MaxCapture        = 24 * time.Hour
	MaxCaptureAge     = 30 * 24 * time.Hour
	MaxGPUs           = 1024
	// Retained is how many captures are kept per deployment; older ones are
	// removed when a new capture is recorded.
	Retained = 8
)

// Frame kinds, root first. A layer a capture could not record is named in
// UnavailableLayers rather than left out silently.
const (
	KindPython  = "python"
	KindTorch   = "torch"
	KindNative  = "native"
	KindCUDAAPI = "cuda_api"
	KindKernel  = "kernel"
	KindSASS    = "sass"
	KindStall   = "stall"
)

// Kinds lists every frame kind in call order.
var Kinds = []string{KindPython, KindTorch, KindNative, KindCUDAAPI, KindKernel, KindSASS, KindStall}

// ErrNotRecorded reports that an existing endpoint has no capture for the
// active revision of any deployment it routes to.
var ErrNotRecorded = errors.New("gpu profile not recorded")

var (
	toolPattern        = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)
	toolVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
)

// CaptureMetadata describes how and when a capture was taken.
type CaptureMetadata struct {
	Tool        string `json:"tool"`
	ToolVersion string `json:"tool_version"`
	// EvidenceClass is always "measured" for a recorded capture.
	EvidenceClass string    `json:"evidence_class"`
	Accelerator   *string   `json:"accelerator"`
	GPUs          *int      `json:"gpus"`
	StartedAt     time.Time `json:"started_at"`
	EndedAt       time.Time `json:"ended_at"`
	// ContentRecorded is always false: no prompt or output enters a profile.
	ContentRecorded bool `json:"content_recorded"`
}

// Capture is CaptureMetadata with the identifier the control plane assigns.
type Capture struct {
	ID string `json:"id"`
	CaptureMetadata
}

// Bucket is sampled GPU time in one timeline bucket. GPUSeconds is nil when
// the bucket has no samples, never zero.
type Bucket struct {
	Start      time.Time `json:"start"`
	GPUSeconds *float64  `json:"gpu_seconds"`
}

// Timeline is sampled GPU time over the capture.
type Timeline struct {
	BucketSeconds int      `json:"bucket_seconds"`
	Buckets       []Bucket `json:"buckets"`
}

// Frame is one interned call-stack frame. File and Line are set only for
// source frames, keyed by function definition rather than call site.
type Frame struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	File string `json:"file,omitempty"`
	Line int    `json:"line,omitempty"`
}

// Stack is GPU time attributed to exactly one root-first path of frame indexes.
type Stack struct {
	Frames []int `json:"frames"`
	GPUNS  int64 `json:"gpu_ns"`
}

// Recording is what a collector sends for one capture.
type Recording struct {
	SchemaVersion     string          `json:"schema_version"`
	Capture           CaptureMetadata `json:"capture"`
	Timeline          Timeline        `json:"timeline"`
	Frames            []Frame         `json:"frames"`
	Stacks            []Stack         `json:"stacks"`
	UnavailableLayers []string        `json:"unavailable_layers"`
}

// Profile is a stored capture as read through an endpoint.
type Profile struct {
	SchemaVersion     string    `json:"schema_version"`
	Endpoint          string    `json:"endpoint"`
	Deployment        string    `json:"deployment"`
	Revision          int       `json:"revision"`
	Capture           Capture   `json:"capture"`
	Timeline          Timeline  `json:"timeline"`
	Frames            []Frame   `json:"frames"`
	Stacks            []Stack   `json:"stacks"`
	UnavailableLayers []string  `json:"unavailable_layers"`
	RecordedAt        time.Time `json:"recorded_at"`
}

// Receipt confirms which deployment revision a recorded capture belongs to.
type Receipt struct {
	ID         string    `json:"id"`
	Deployment string    `json:"deployment"`
	Revision   int       `json:"revision"`
	StartedAt  time.Time `json:"started_at"`
	EndedAt    time.Time `json:"ended_at"`
	Frames     int       `json:"frames"`
	Stacks     int       `json:"stacks"`
	RecordedAt time.Time `json:"recorded_at"`
}

// Normalize returns the recording with UTC, microsecond timestamps and
// non-nil lists, so a retry hashes and compares identically.
func Normalize(recording Recording) Recording {
	recording.Capture.StartedAt = recording.Capture.StartedAt.UTC().Truncate(time.Microsecond)
	recording.Capture.EndedAt = recording.Capture.EndedAt.UTC().Truncate(time.Microsecond)
	buckets := make([]Bucket, len(recording.Timeline.Buckets))
	for index, bucket := range recording.Timeline.Buckets {
		bucket.Start = bucket.Start.UTC().Truncate(time.Microsecond)
		buckets[index] = bucket
	}
	recording.Timeline.Buckets = buckets
	if recording.Frames == nil {
		recording.Frames = []Frame{}
	}
	if recording.Stacks == nil {
		recording.Stacks = []Stack{}
	}
	if recording.UnavailableLayers == nil {
		recording.UnavailableLayers = []string{}
	}
	return recording
}

// Validate rejects a recording that is unbounded, inconsistent, claims
// content, or is not a measurement. now bounds the capture time.
func Validate(recording Recording, now time.Time) error {
	if recording.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schema_version must be %s", SchemaVersion)
	}
	if err := validateCapture(recording.Capture, now); err != nil {
		return err
	}
	if err := validateTimeline(recording.Timeline, recording.Capture); err != nil {
		return err
	}
	if len(recording.Frames) < 1 || len(recording.Frames) > MaxFrames {
		return fmt.Errorf("frames must contain 1..%d entries", MaxFrames)
	}
	for index, frame := range recording.Frames {
		if err := validateFrame(frame); err != nil {
			return fmt.Errorf("frame %d: %w", index, err)
		}
	}
	if len(recording.Stacks) < 1 || len(recording.Stacks) > MaxStacks {
		return fmt.Errorf("stacks must contain 1..%d entries", MaxStacks)
	}
	limit := int64(recording.Capture.EndedAt.Sub(recording.Capture.StartedAt)) * int64(gpuCount(recording.Capture))
	var total int64
	for index, stack := range recording.Stacks {
		if len(stack.Frames) < 1 || len(stack.Frames) > MaxStackDepth {
			return fmt.Errorf("stack %d must contain 1..%d frames", index, MaxStackDepth)
		}
		for _, frame := range stack.Frames {
			if frame < 0 || frame >= len(recording.Frames) {
				return fmt.Errorf("stack %d names frame %d, which does not exist", index, frame)
			}
		}
		if stack.GPUNS < 1 || stack.GPUNS > limit {
			return fmt.Errorf("stack %d gpu_ns must be positive and fit within the capture", index)
		}
		total += stack.GPUNS
		if total > limit {
			return errors.New("stacks attribute more GPU time than the capture holds")
		}
	}
	seen := map[string]bool{}
	for _, layer := range recording.UnavailableLayers {
		if !knownKind(layer) || seen[layer] {
			return fmt.Errorf("unavailable_layers must name distinct frame kinds; %q is not one", layer)
		}
		seen[layer] = true
	}
	return nil
}

func validateCapture(capture CaptureMetadata, now time.Time) error {
	if !toolPattern.MatchString(capture.Tool) || !toolVersionPattern.MatchString(capture.ToolVersion) {
		return errors.New("capture tool and tool_version must be bounded identifiers")
	}
	if capture.EvidenceClass != "measured" {
		return errors.New("capture evidence_class must be measured")
	}
	if capture.ContentRecorded {
		return errors.New("capture content_recorded must be false; a GPU profile never records prompts or outputs")
	}
	if capture.Accelerator != nil && !boundedText(*capture.Accelerator, 128) {
		return errors.New("capture accelerator must be 1..128 printable characters")
	}
	if capture.GPUs != nil && (*capture.GPUs < 1 || *capture.GPUs > MaxGPUs) {
		return fmt.Errorf("capture gpus must be 1..%d", MaxGPUs)
	}
	if capture.StartedAt.IsZero() || !capture.EndedAt.After(capture.StartedAt) || capture.EndedAt.Sub(capture.StartedAt) > MaxCapture {
		return errors.New("capture must end after it starts and last at most 24 hours")
	}
	if capture.EndedAt.After(now.Add(time.Minute)) || capture.StartedAt.Before(now.Add(-MaxCaptureAge)) {
		return errors.New("capture must be from the last 30 days and not in the future")
	}
	return nil
}

func validateTimeline(timeline Timeline, capture CaptureMetadata) error {
	if timeline.BucketSeconds < 1 || timeline.BucketSeconds > 3600 {
		return errors.New("timeline bucket_seconds must be 1..3600")
	}
	if len(timeline.Buckets) > MaxBuckets {
		return fmt.Errorf("timeline must contain at most %d buckets", MaxBuckets)
	}
	ceiling := float64(timeline.BucketSeconds) * float64(gpuCount(capture))
	for index, bucket := range timeline.Buckets {
		if bucket.Start.Before(capture.StartedAt) || !bucket.Start.Before(capture.EndedAt) {
			return fmt.Errorf("timeline bucket %d must start inside the capture", index)
		}
		if index > 0 && !bucket.Start.After(timeline.Buckets[index-1].Start) {
			return errors.New("timeline buckets must be in increasing start order")
		}
		if value := bucket.GPUSeconds; value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 || *value > ceiling) {
			return fmt.Errorf("timeline bucket %d gpu_seconds must be finite, not negative, and fit in the bucket", index)
		}
	}
	return nil
}

func validateFrame(frame Frame) error {
	if !knownKind(frame.Kind) {
		return fmt.Errorf("kind must be one of %s", strings.Join(Kinds, ", "))
	}
	if !boundedText(frame.Name, MaxFrameName) {
		return fmt.Errorf("name must be 1..%d printable characters", MaxFrameName)
	}
	if frame.File != "" && !boundedText(frame.File, MaxFrameFile) {
		return fmt.Errorf("file must be at most %d printable characters", MaxFrameFile)
	}
	if frame.Line < 0 || frame.Line > 10_000_000 || frame.Line > 0 && frame.File == "" {
		return errors.New("line must be positive and needs a file")
	}
	return nil
}

// gpuCount is the declared GPU count, or the largest allowed when unknown, so
// an undeclared count never rejects a valid multi-GPU capture.
func gpuCount(capture CaptureMetadata) int {
	if capture.GPUs != nil {
		return *capture.GPUs
	}
	return MaxGPUs
}

func knownKind(kind string) bool {
	for _, known := range Kinds {
		if kind == known {
			return true
		}
	}
	return false
}

func boundedText(value string, limit int) bool {
	if value == "" || len(value) > limit || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

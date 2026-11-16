package gpuprofile

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func valid(now time.Time) Recording {
	started := now.Add(-2 * time.Minute)
	one, half := 1.0, .5
	gpus := 1
	accelerator := "NVIDIA L40S"
	return Recording{
		SchemaVersion: SchemaVersion,
		Capture:       CaptureMetadata{Tool: "cupti-pc-sampling", ToolVersion: "12.8", EvidenceClass: "measured", Accelerator: &accelerator, GPUs: &gpus, StartedAt: started, EndedAt: started.Add(time.Minute)},
		Timeline:      Timeline{BucketSeconds: 30, Buckets: []Bucket{{Start: started, GPUSeconds: &one}, {Start: started.Add(30 * time.Second), GPUSeconds: nil}, {Start: started.Add(45 * time.Second), GPUSeconds: &half}}},
		Frames: []Frame{
			{Kind: KindPython, Name: "LlamaMLP.forward", File: "vllm/model_executor/models/llama.py", Line: 92},
			{Kind: KindKernel, Name: "sm90_xmma_gemm_bf16"},
			{Kind: KindStall, Name: "long_scoreboard"},
		},
		Stacks:            []Stack{{Frames: []int{0, 1, 2}, GPUNS: int64(20 * time.Second)}, {Frames: []int{0, 1}, GPUNS: int64(5 * time.Second)}},
		UnavailableLayers: []string{KindSASS},
	}
}

func TestValidRecordingPasses(t *testing.T) {
	now := time.Now().UTC()
	if err := Validate(valid(now), now); err != nil {
		t.Fatal(err)
	}
	unknownCount := valid(now)
	unknownCount.Capture.GPUs, unknownCount.Capture.Accelerator = nil, nil
	if err := Validate(unknownCount, now); err != nil {
		t.Fatalf("an undeclared GPU count or accelerator stays unknown, not invalid: %v", err)
	}
}

func TestValidateRejectsUnboundedOrDishonestRecordings(t *testing.T) {
	now := time.Now().UTC()
	cases := map[string]func(*Recording){
		"wrong schema":           func(r *Recording) { r.SchemaVersion = "infercrane.gpu-profile/v2" },
		"fixture evidence":       func(r *Recording) { r.Capture.EvidenceClass = "fixture" },
		"content recorded":       func(r *Recording) { r.Capture.ContentRecorded = true },
		"tool with spaces":       func(r *Recording) { r.Capture.Tool = "nsight compute" },
		"ends before start":      func(r *Recording) { r.Capture.EndedAt = r.Capture.StartedAt },
		"future capture":         func(r *Recording) { r.Capture.EndedAt = now.Add(time.Hour) },
		"capture too old":        func(r *Recording) { r.Capture.StartedAt = now.Add(-31 * 24 * time.Hour) },
		"capture too long":       func(r *Recording) { r.Capture.StartedAt = r.Capture.EndedAt.Add(-25 * time.Hour) },
		"zero gpus":              func(r *Recording) { zero := 0; r.Capture.GPUs = &zero },
		"blank accelerator":      func(r *Recording) { blank := " "; r.Capture.Accelerator = &blank },
		"bucket before capture":  func(r *Recording) { r.Timeline.Buckets[0].Start = r.Capture.StartedAt.Add(-time.Second) },
		"buckets out of order":   func(r *Recording) { r.Timeline.Buckets[2].Start = r.Timeline.Buckets[1].Start },
		"bucket over its length": func(r *Recording) { over := 31.0; r.Timeline.Buckets[0].GPUSeconds = &over },
		"negative bucket":        func(r *Recording) { negative := -1.0; r.Timeline.Buckets[0].GPUSeconds = &negative },
		"no frames":              func(r *Recording) { r.Frames = nil },
		"unknown kind":           func(r *Recording) { r.Frames[1].Kind = "warp" },
		"control character":      func(r *Recording) { r.Frames[1].Name = "gemm\nprompt" },
		"overlong name":          func(r *Recording) { r.Frames[1].Name = strings.Repeat("k", MaxFrameName+1) },
		"line without file":      func(r *Recording) { r.Frames[1].Line = 3 },
		"no stacks":              func(r *Recording) { r.Stacks = nil },
		"empty stack":            func(r *Recording) { r.Stacks[0].Frames = nil },
		"missing frame":          func(r *Recording) { r.Stacks[0].Frames = []int{0, 3} },
		"zero time":              func(r *Recording) { r.Stacks[0].GPUNS = 0 },
		"more time than capture": func(r *Recording) { r.Stacks[1].GPUNS = int64(41 * time.Second) },
		"unknown layer":          func(r *Recording) { r.UnavailableLayers = []string{"memory"} },
		"repeated layer":         func(r *Recording) { r.UnavailableLayers = []string{KindSASS, KindSASS} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			recording := valid(now)
			mutate(&recording)
			if err := Validate(recording, now); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	deep := valid(now)
	deep.Stacks[0].Frames = make([]int, MaxStackDepth+1)
	if err := Validate(deep, now); err == nil {
		t.Fatal("accepted a stack deeper than the bound")
	}
}

func TestNormalizeMakesRetriesCompareEqual(t *testing.T) {
	now := time.Now().UTC()
	local := valid(now)
	local.Capture.StartedAt = local.Capture.StartedAt.In(time.FixedZone("x", 3600)).Add(time.Nanosecond)
	local.UnavailableLayers = nil
	utc := valid(now)
	utc.UnavailableLayers = []string{}
	left, _ := json.Marshal(Normalize(local))
	right, _ := json.Marshal(Normalize(utc))
	if string(left) != string(right) {
		t.Fatalf("normalized recordings differ:\n%s\n%s", left, right)
	}
	if !strings.Contains(string(left), `"unavailable_layers":[]`) {
		t.Fatalf("empty lists must encode as [], got %s", left)
	}
}

func TestProfileEncodesTheDashboardShape(t *testing.T) {
	now := time.Now().UTC()
	recording := Normalize(valid(now))
	profile := Profile{SchemaVersion: SchemaVersion, Endpoint: "coder", Deployment: "coder", Revision: 3, Capture: Capture{ID: "gpc-1", CaptureMetadata: recording.Capture}, Timeline: recording.Timeline, Frames: recording.Frames, Stacks: recording.Stacks, UnavailableLayers: recording.UnavailableLayers, RecordedAt: now}
	encoded, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"capture":{"id":"gpc-1","tool":"cupti-pc-sampling"`, `"gpu_seconds":null`, `"content_recorded":false`, `"revision":3`, `"frames":[0,1,2]`} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("missing %s in %s", want, encoded)
		}
	}
	if strings.Contains(string(encoded), `"file":""`) || strings.Contains(string(encoded), `"line":0`) {
		t.Fatalf("kernel frames must omit file and line: %s", encoded)
	}
}

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/gpuprofile"
)

func gpuProfileRecording(started time.Time, kernel string) gpuprofile.Recording {
	gpus := 1
	seconds := 1.5
	return gpuprofile.Recording{
		SchemaVersion: gpuprofile.SchemaVersion,
		Capture:       gpuprofile.CaptureMetadata{Tool: "cupti-pc-sampling", ToolVersion: "12.8", EvidenceClass: "measured", GPUs: &gpus, StartedAt: started, EndedAt: started.Add(time.Minute)},
		Timeline:      gpuprofile.Timeline{BucketSeconds: 30, Buckets: []gpuprofile.Bucket{{Start: started, GPUSeconds: &seconds}, {Start: started.Add(30 * time.Second)}}},
		Frames:        []gpuprofile.Frame{{Kind: gpuprofile.KindPython, Name: "LlamaMLP.forward", File: "llama.py", Line: 92}, {Kind: gpuprofile.KindKernel, Name: kernel}, {Kind: gpuprofile.KindStall, Name: "long_scoreboard"}},
		Stacks:        []gpuprofile.Stack{{Frames: []int{0, 1, 2}, GPUNS: int64(10 * time.Second)}, {Frames: []int{0, 1}, GPUNS: int64(2 * time.Second)}},
	}
}

func TestGPUProfilesArePinnedToTheActiveRevisionImmutableAndBounded(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	name := "profiled-" + time.Now().UTC().Format("150405.000000000")
	target := name + "-target"
	if _, err := s.AddTarget(ctx, domain.Target{Name: target, URL: "http://profiled.invalid", Provider: "existing", Runtime: "vllm", UpstreamModel: "org/model"}); err != nil {
		t.Fatal(err)
	}
	deployment, err := s.ApplyDeployment(ctx, domain.Deployment{Name: name, Model: "org/model", Runtime: "vllm", MinReplicas: 1, MaxReplicas: 1}, []string{target})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = s.EndpointGPUProfile(ctx, "global", name); !errors.Is(err, gpuprofile.ErrNotRecorded) {
		t.Fatalf("before any capture err=%v", err)
	}
	if _, err = s.EndpointGPUProfile(ctx, "global", name+"-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown endpoint err=%v", err)
	}

	started := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	recording := gpuProfileRecording(started, "sm90_xmma_gemm_bf16")
	receipt, err := s.RecordGPUProfile(ctx, "global", deployment.ID, recording)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Deployment != name || receipt.Revision != 1 || receipt.Frames != 3 || receipt.Stacks != 2 || len(receipt.ID) != 36 {
		t.Fatalf("receipt=%+v", receipt)
	}
	retried, err := s.RecordGPUProfile(ctx, "global", name, recording)
	if err != nil || retried.ID != receipt.ID || !retried.RecordedAt.Equal(receipt.RecordedAt) {
		t.Fatalf("idempotent retry receipt=%+v retried=%+v err=%v", receipt, retried, err)
	}
	if _, err = s.RecordGPUProfile(ctx, "global", name, gpuProfileRecording(started, "a_different_kernel")); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed retry err=%v", err)
	}
	if _, err = s.RecordGPUProfile(ctx, "other", name, recording); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant write err=%v", err)
	}
	if _, err = s.EndpointGPUProfile(ctx, "other", name); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant read err=%v", err)
	}
	invalid := gpuProfileRecording(started.Add(time.Hour), "gemm")
	invalid.Capture.ContentRecorded = true
	if _, err = s.RecordGPUProfile(ctx, "global", name, invalid); err == nil {
		t.Fatal("a capture that claims content was stored")
	}

	profile, err := s.EndpointGPUProfile(ctx, "global", name)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Endpoint != name || profile.Deployment != name || profile.Revision != 1 || profile.Capture.ID != receipt.ID || profile.SchemaVersion != gpuprofile.SchemaVersion {
		t.Fatalf("profile identity=%+v", profile)
	}
	if len(profile.Frames) != 3 || profile.Frames[0].Line != 92 || profile.Stacks[0].GPUNS != int64(10*time.Second) || profile.Timeline.Buckets[1].GPUSeconds != nil || *profile.Timeline.Buckets[0].GPUSeconds != 1.5 {
		t.Fatalf("profile samples=%+v", profile)
	}
	if !profile.Capture.StartedAt.Equal(started) || profile.Capture.ContentRecorded || len(profile.UnavailableLayers) != 0 || profile.UnavailableLayers == nil {
		t.Fatalf("profile capture=%+v layers=%v", profile.Capture, profile.UnavailableLayers)
	}

	for index := 1; index <= gpuprofile.Retained+2; index++ {
		if _, err = s.RecordGPUProfile(ctx, "global", name, gpuProfileRecording(started.Add(time.Duration(index)*2*time.Minute), "gemm")); err != nil {
			t.Fatal(err)
		}
	}
	var kept int
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM gpu_profiles WHERE deployment_id=$1`, deployment.ID).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != gpuprofile.Retained {
		t.Fatalf("kept %d captures, want %d", kept, gpuprofile.Retained)
	}
	latest, err := s.EndpointGPUProfile(ctx, "global", name)
	if err != nil || !latest.Capture.StartedAt.Equal(started.Add(time.Duration(gpuprofile.Retained+2)*2*time.Minute)) {
		t.Fatalf("latest=%+v err=%v", latest.Capture, err)
	}

	candidate, err := s.CreateCandidateRevision(ctx, "global", name, `{"model":"org/model","runtime":"vllm","routing_strategy":"round-robin","min_replicas":1,"max_replicas":1,"autoscaling_enabled":false}`)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.PromoteCandidateRevision(ctx, "global", name, candidate.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.EndpointGPUProfile(ctx, "global", name); !errors.Is(err, gpuprofile.ErrNotRecorded) {
		t.Fatalf("a capture of the previous revision was served after a rollout: err=%v", err)
	}
	rolled, err := s.RecordGPUProfile(ctx, "global", name, gpuProfileRecording(started, "gemm"))
	if err != nil || rolled.Revision != candidate.Number {
		t.Fatalf("capture after rollout receipt=%+v candidate=%d err=%v", rolled, candidate.Number, err)
	}
}

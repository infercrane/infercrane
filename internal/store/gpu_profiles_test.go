package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/gpuprofile"
	"github.com/infercrane/infercrane/internal/gputrace"
	"github.com/infercrane/infercrane/internal/testtools/tracefixture"
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

// The capture history keeps captures of revisions that no longer serve, so a
// capture before a rollout compares with one after it; reading by ID is
// scoped to the endpoint's deployments; expired captures are neither listed
// nor read.
func TestGPUProfileHistoryOutlivesARolloutAndExpires(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	name := "history-" + time.Now().UTC().Format("150405.000000000")
	other := name + "-other"
	for _, deploymentName := range []string{name, other} {
		target := deploymentName + "-target"
		if _, err := s.AddTarget(ctx, domain.Target{Name: target, URL: "http://" + deploymentName + ".invalid", Provider: "existing", Runtime: "vllm", UpstreamModel: "org/model"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ApplyDeployment(ctx, domain.Deployment{Name: deploymentName, Model: "org/model", Runtime: "vllm", MinReplicas: 1, MaxReplicas: 1}, []string{target}); err != nil {
			t.Fatal(err)
		}
	}
	if list, err := s.EndpointGPUProfiles(ctx, "global", name); err != nil || len(list) != 0 || list == nil {
		t.Fatalf("an endpoint without captures lists none: %v %v", list, err)
	}
	if _, err := s.EndpointGPUProfiles(ctx, "global", name+"-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown endpoint err=%v", err)
	}
	started := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	before, err := s.RecordGPUProfile(ctx, "global", name, gpuProfileRecording(started, "gemm_before"))
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := s.CreateCandidateRevision(ctx, "global", name, `{"model":"org/model","runtime":"vllm","routing_strategy":"round-robin","min_replicas":1,"max_replicas":1,"autoscaling_enabled":false}`)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.PromoteCandidateRevision(ctx, "global", name, candidate.ID); err != nil {
		t.Fatal(err)
	}
	after, err := s.RecordGPUProfile(ctx, "global", name, gpuProfileRecording(started.Add(time.Hour), "gemm_after"))
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := s.RecordGPUProfile(ctx, "global", other, gpuProfileRecording(started, "gemm_other"))
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.EndpointGPUProfiles(ctx, "global", name)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != after.ID || !list[0].Active || list[1].ID != before.ID || list[1].Active || list[1].Revision != 1 || list[0].Revision != candidate.Number {
		t.Fatalf("history %+v", list)
	}
	if list[0].Tool != "cupti-pc-sampling" || list[0].GPUs == nil || *list[0].GPUs != 1 || list[0].Verdict != nil || list[0].BusyShare != nil || list[0].SchemaVersion != gpuprofile.SchemaVersion {
		t.Fatalf("a v1 summary has its capture and no verdict: %+v", list[0])
	}
	old, active, err := s.EndpointGPUProfileByID(ctx, "global", name, before.ID)
	if err != nil || active || old.Revision != 1 || old.Frames[1].Name != "gemm_before" || old.Capture.ID != before.ID || old.Endpoint != name {
		t.Fatalf("read by ID profile=%+v active=%v err=%v", old.Capture, active, err)
	}
	if _, _, err = s.EndpointGPUProfileByID(ctx, "global", name, foreign.ID); !errors.Is(err, gpuprofile.ErrNotRecorded) {
		t.Fatalf("another endpoint's capture is not readable here: %v", err)
	}
	if _, _, err = s.EndpointGPUProfileByID(ctx, "other", name, before.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant read err=%v", err)
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE gpu_profiles SET ended_at=NOW()-INTERVAL '31 days',started_at=NOW()-INTERVAL '31 days 1 minute' WHERE id=$1`, before.ID); err != nil {
		t.Fatal(err)
	}
	if list, err = s.EndpointGPUProfiles(ctx, "global", name); err != nil || len(list) != 1 || list[0].ID != after.ID {
		t.Fatalf("an expired capture is not listed: %+v %v", list, err)
	}
	if _, _, err = s.EndpointGPUProfileByID(ctx, "global", name, before.ID); !errors.Is(err, gpuprofile.ErrNotRecorded) {
		t.Fatalf("an expired capture is not readable: %v", err)
	}
	if _, err = s.RecordGPUProfile(ctx, "global", name, gpuProfileRecording(started.Add(90*time.Minute), "gemm_later")); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM gpu_profiles WHERE id=$1`, before.ID).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("recording a capture removes expired ones: remaining=%d err=%v", remaining, err)
	}
}

func TestAnalyzedGPUProfileKeepsItsAnalysisAndExcerpt(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	name := "analyzed-" + time.Now().UTC().Format("150405.000000000")
	target := name + "-target"
	if _, err := s.AddTarget(ctx, domain.Target{Name: target, URL: "http://analyzed.invalid", Provider: "existing", Runtime: "vllm", UpstreamModel: "org/model"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyDeployment(ctx, domain.Deployment{Name: name, Model: "org/model", Runtime: "vllm", MinReplicas: 1, MaxReplicas: 1}, []string{target}); err != nil {
		t.Fatal(err)
	}
	trace, err := gputrace.ParseKineto(bytes.NewReader(tracefixture.VLLMSingleGPU(time.Now().UTC().Add(-time.Hour))))
	if err != nil {
		t.Fatal(err)
	}
	recording, err := gputrace.Profile(trace, gputrace.Options{Engine: "vllm"}, gputrace.CaptureFromTrace(trace, "torch-profiler", "vllm-0.22.1", time.Time{}))
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := s.RecordGPUProfile(ctx, "global", name, recording)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := s.EndpointGPUProfile(ctx, "global", name)
	if err != nil {
		t.Fatal(err)
	}
	if profile.SchemaVersion != gpuprofile.SchemaVersionV2 || profile.Analysis == nil || profile.TimelineExcerpt == nil || profile.Analysis.Verdict != recording.Analysis.Verdict || len(profile.TimelineExcerpt.Events) != len(recording.TimelineExcerpt.Events) {
		t.Fatalf("the stored analysis must read back unchanged: %+v", profile.Analysis)
	}
	list, err := s.EndpointGPUProfiles(ctx, "global", name)
	if err != nil || len(list) != 1 || list[0].ID != receipt.ID || list[0].Verdict == nil || *list[0].Verdict != recording.Analysis.Verdict || list[0].BusyShare == nil || *list[0].BusyShare != recording.Analysis.Utilization.BusyShare {
		t.Fatalf("summary %+v err=%v", list, err)
	}
}

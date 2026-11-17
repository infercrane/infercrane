package metrics

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestFilterEngineExpositionKeepsOnlyParsedFamilies(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "*.prom"))
	if err != nil || len(files) == 0 {
		t.Fatalf("fixtures: %v %v", files, err)
	}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw) + "\n# HELP app_private something else\napp_private_queue{prompt=\"secret words\"} 4\nprocess_open_fds 12\n"
		original, parseErr := ParseEngine(text)
		filtered, engine, err := FilterEngineExposition(text)
		if errors.Is(parseErr, ErrNoEngineMetrics) {
			if !errors.Is(err, ErrNoEngineMetrics) {
				t.Fatalf("%s: filter error %v, want ErrNoEngineMetrics", file, err)
			}
			continue
		}
		if parseErr != nil {
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if engine != original.Engine || strings.Contains(filtered, "#") || strings.Contains(filtered, "secret words") || strings.Contains(filtered, "process_open_fds") {
			t.Fatalf("%s: engine=%s filtered=\n%s", file, engine, filtered)
		}
		reparsed, err := ParseEngine(filtered)
		if err != nil {
			t.Fatalf("%s: filtered text does not parse: %v", file, err)
		}
		if !reflect.DeepEqual(reparsed, original) {
			t.Fatalf("%s: filtering changed the parsed snapshot\nbefore %#v\nafter  %#v", file, original, reparsed)
		}
	}
}

func TestFilterEngineExpositionRefusesPayloadsWithoutAnEngine(t *testing.T) {
	if _, _, err := FilterEngineExposition("process_open_fds 3\n"); !errors.Is(err, ErrNoEngineMetrics) {
		t.Fatalf("err=%v", err)
	}
	if _, _, err := FilterEngineExposition(strings.Repeat("x", MaxEnginePayloadBytes+1)); err == nil {
		t.Fatal("oversized payload accepted")
	}
	if _, _, err := FilterEngineExposition("vllm:num_requests_running 1\nsglang:num_running_reqs 1\n"); err == nil {
		t.Fatal("mixed engines accepted")
	}
}

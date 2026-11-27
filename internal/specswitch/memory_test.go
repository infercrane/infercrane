package specswitch

import (
	"strings"
	"testing"
)

func TestParseProbeReadsNvidiaSmisCSV(t *testing.T) {
	used, total, err := ParseProbe(" 1203, 81559\n")
	if err != nil || used != 1203 || total != 81559 {
		t.Fatalf("got %v %v %v", used, total, err)
	}
	for _, bad := range []string{"", "1203", "1203, 81559\n900, 81559", "x, 81559", "90000, 81559", "-1, 81559"} {
		if _, _, err := ParseProbe(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestMemoryShareLeavesRoomForTheSleepingEngines(t *testing.T) {
	// An H100 (81,559 MiB): one engine asleep keeps 900 MiB; two will sleep
	// beside the awake one, with 512 MiB of margin.
	share, err := MemoryShare(900, 81559, 1, 2, 512, 0.98)
	if err != nil {
		t.Fatal(err)
	}
	if want := 0.971; share != want {
		t.Fatalf("share %.3f, want %.3f", share, want)
	}
	awake := share * 81559
	if awake+2*900+512 > 81559 {
		t.Fatalf("an awake engine of %.0f MiB and two sleeping ones do not fit", awake)
	}
	if capped, _ := MemoryShare(100, 81559, 1, 2, 0, 0.95); capped != 0.95 {
		t.Fatalf("share above the cap: %.3f", capped)
	}
	if _, err := MemoryShare(30000, 81559, 1, 2, 512, 0.98); err == nil || !strings.Contains(err.Error(), "keep 30000 MiB") {
		t.Fatalf("a share under half the GPU was given: %v", err)
	}
}

func TestStartOrderPutsOwnShareEnginesFirst(t *testing.T) {
	// drafts-3 (the first band's) and drafts-1 are sized; drafts-0 has its own
	// share. The last started (drafts-1) is awake until the command wakes drafts-3.
	cfg := validConfig()
	cfg.Engines = append(cfg.Engines, Engine{Name: "drafts-0", URL: "http://127.0.0.1:8103", Command: []string{"vllm", "--gpu-memory-utilization", "0.80"}})
	cfg.Engines[0].Command = []string{"vllm", "--gpu-memory-utilization", MemoryShareToken}
	cfg.Engines[1].Command = []string{"vllm", "--gpu-memory-utilization", MemoryShareToken}
	cfg.Memory = Memory{Probe: []string{"nvidia-smi"}, MarginMiB: 512, MaxShare: 0.98}
	if _, err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, engine := range StartOrder(cfg) {
		names = append(names, engine.Name)
	}
	if strings.Join(names, ",") != "drafts-0,drafts-3,drafts-1" {
		t.Fatalf("order %v", names)
	}
	if got := WithShare(cfg.Engines[0].Command, 0.971); got[2] != "0.971" || cfg.Engines[0].Command[2] != MemoryShareToken {
		t.Fatalf("share not put in place, or the config changed: %v", got)
	}
	// Without an engine of its own share nothing can be measured first.
	cfg.Engines = cfg.Engines[:2]
	if _, err := cfg.Normalize(); err == nil {
		t.Fatal("a configuration whose every engine takes the measured share was accepted")
	}
	cfg = validConfig()
	cfg.Engines[1].Command = []string{"vllm", "--gpu-memory-utilization", MemoryShareToken}
	if _, err := cfg.Normalize(); err == nil {
		t.Fatal("a sized engine without a memory probe was accepted")
	}
}

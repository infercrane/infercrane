package optimizer

import "testing"

func TestBoundedDiagnosticKeepsTheLastTracebackLine(t *testing.T) {
	traceback := "Traceback (most recent call last):\n  File \"<string>\", line 1, in <module>\nModuleNotFoundError: No module named 'aiconfigurator'"
	if got := boundedDiagnostic(traceback); got != "ModuleNotFoundError: No module named 'aiconfigurator'" {
		t.Fatalf("boundedDiagnostic = %q", got)
	}
	if got := boundedDiagnostic("  plain failure  "); got != "plain failure" {
		t.Fatalf("boundedDiagnostic = %q", got)
	}
}

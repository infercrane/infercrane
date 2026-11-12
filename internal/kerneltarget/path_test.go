package kerneltarget

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/kernelgen"
	"github.com/infercrane/infercrane/internal/kernelplanner"
)

func TestRequestDerivedPathsStayInsideTheirDirectories(t *testing.T) {
	target := Target{ModelCache: "/cache", ReplayDir: "/replays", PackDir: "/packs"}
	snapshot := target.snapshotPath(kernelplanner.ModelIdentity{Repository: "../../etc", Revision: "../../../passwd"})
	if !strings.HasPrefix(snapshot, "/cache/") {
		t.Fatalf("snapshot path escaped the cache: %s", snapshot)
	}
	if _, err := target.contentAddressedFile("replay@sha256:../../etc/passwd"); err == nil {
		t.Fatal("non-digest identity accepted")
	}
	if path, err := target.packIntegration(kernelgen.Pack{Family: "../../x", Integration: "../../../etc", Runtimes: []string{"vllm"}, RuntimeVersions: map[string][]string{"vllm": {"0.22.1"}}}, RuntimeSpec{Name: "vllm", Version: "0.22.1"}); err == nil || strings.HasPrefix(path, "/etc") {
		t.Fatalf("integration path escaped the pack directory: %q err=%v", path, err)
	}
	if got := filepath.Join(target.PackDir, filepath.Clean("/"+"../../x")); !strings.HasPrefix(got, "/packs/") {
		t.Fatalf("rooting did not contain the path: %s", got)
	}
}

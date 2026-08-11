package main

import "testing"

func TestSkipRepositoryDirectoryExcludesRuntimeAndGeneratedTrees(t *testing.T) {
	for _, path := range []string{".git", ".infercrane", "bin", "docs/.mintlify", "docs/node_modules"} {
		if !skipRepositoryDirectory(path) {
			t.Fatalf("runtime or generated directory %q is not excluded", path)
		}
	}
	for _, path := range []string{"cmd", "internal", "docs", "tools"} {
		if skipRepositoryDirectory(path) {
			t.Fatalf("source directory %q was excluded", path)
		}
	}
}

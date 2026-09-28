package modeloptcatalog

import "testing"

func TestEmbeddedCatalogIsImmutableAndHardwareBound(t *testing.T) {
	catalog, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Seeds) != 3 {
		t.Fatalf("seeds=%d", len(catalog.Seeds))
	}
	if got := catalog.Match("Qwen/Qwen3.8-27B", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "vllm", "H200"); len(got) != 0 {
		t.Fatalf("Blackwell checkpoint leaked into Hopper: %+v", got)
	}
	got := catalog.Match("Qwen/Qwen3.8-27B", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "vllm", "B200")
	if len(got) != 1 || got[0].OutputRevision != "482ca0f3832238542f8f5295dde86b5f22711d80" || got[0].LineageState != "publisher_declared" {
		t.Fatalf("Qwen seed mismatch: %+v", got)
	}
	if got := catalog.Match("deepseek-ai/DeepSeek-V4.1-Flash", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "sglang", "GB300"); len(got) != 0 {
		t.Fatalf("exact lineage accepted the wrong base revision: %+v", got)
	}
}

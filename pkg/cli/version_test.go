package cli

import "testing"

func TestVCSVersion(t *testing.T) {
	if v, ok := vcsVersion("", false); ok || v != "" {
		t.Fatalf("empty revision should report no version, got %q ok=%v", v, ok)
	}
	if v, ok := vcsVersion("48deeb", false); ok {
		t.Fatalf("too-short revision should report no version, got %q ok=%v", v, ok)
	}
	if v, ok := vcsVersion("48deeb2abc123def4567890123456789012345678", false); !ok || v != "dev-48deeb2" {
		t.Fatalf("full revision should shorten to dev-<sha7>, got %q ok=%v", v, ok)
	}
	if v, ok := vcsVersion("48deeb2abc123def4567890123456789012345678", true); !ok || v != "dev-48deeb2-dirty" {
		t.Fatalf("modified checkout should carry -dirty, got %q ok=%v", v, ok)
	}
}

package cli

import (
	"strings"
	"testing"
)

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

func TestVersionInfoTable(t *testing.T) {
	full := versionInfo{Version: "v0.2.0", GoVersion: "go1.27.3", Commit: "abc123", Built: "now", GOOS: "linux", GOARCH: "amd64"}
	out := full.table()
	for _, want := range []string{
		"Version:      v0.2.0\n",
		"Go Version:   go1.27.3\n",
		"Git Commit:   abc123\n",
		"Built:        now\n",
		"OS/Arch:      linux/amd64\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}
	minimal := versionInfo{Version: "dev", GoVersion: "go1.27.3", GOOS: "linux", GOARCH: "amd64"}.table()
	if strings.Contains(minimal, "Git Commit") || strings.Contains(minimal, "Built:") {
		t.Errorf("unstamped build should omit commit/built lines:\n%s", minimal)
	}
}

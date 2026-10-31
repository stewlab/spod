package engine

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestNeededStages(t *testing.T) {
	cf, err := parseContainerfile(strings.NewReader(`ARG B=base
FROM alpine AS base
FROM alpine AS unused
FROM $B AS mid
FROM alpine AS tools
FROM scratch
COPY --from=mid /a /a
COPY --from=3 /b /b
`))
	if err != nil {
		t.Fatal(err)
	}
	b := &builder{cf: cf, escape: cf.Escape, stageByNm: map[string]int{}}
	for i, s := range cf.Stages {
		if s.Name != "" {
			b.stageByNm[s.Name] = i
		}
	}
	if err := b.resolveGlobals(); err != nil {
		t.Fatal(err)
	}
	if got, want := b.neededStages(4), map[int]bool{0: true, 2: true, 3: true, 4: true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("needed %v, want %v", got, want)
	}
}

func TestBuildCopyDestinations(t *testing.T) {
	ctx, rootfs := t.TempDir(), t.TempDir()
	for name, data := range map[string]string{"a.txt": "a", "b.txt": "b", "dir/c": "c", "skip.txt": "s"} {
		p := filepath.Join(ctx, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(rootfs, "existing"), 0o755); err != nil {
		t.Fatal(err)
	}
	// An absolute symlink must resolve inside the rootfs.
	if err := os.Symlink("/existing", filepath.Join(rootfs, "link")); err != nil {
		t.Fatal(err)
	}
	uid, gid := os.Getuid(), os.Getgid()
	copyOp := func(dest string, ignore []string, sources ...string) error {
		return buildCopy(buildCopyPayload{Rootfs: rootfs, Dest: dest, SrcRoot: ctx, Sources: sources, Ignore: ignore, UID: uid, GID: gid})
	}
	for _, step := range []struct {
		dest    string
		ignore  []string
		sources []string
	}{
		{"/renamed.txt", nil, []string{"a.txt"}},
		{"/existing", nil, []string{"a.txt"}},
		{"/new/", []string{"skip.txt"}, []string{"*.txt"}},
		{"/tree", nil, []string{"dir"}},
		{"/link/", nil, []string{"b.txt"}},
	} {
		if err := copyOp(step.dest, step.ignore, step.sources...); err != nil {
			t.Fatalf("copy %v -> %s: %v", step.sources, step.dest, err)
		}
	}
	for _, want := range []string{"renamed.txt", "existing/a.txt", "new/a.txt", "new/b.txt", "tree/c", "existing/b.txt"} {
		if _, err := os.Stat(filepath.Join(rootfs, want)); err != nil {
			t.Errorf("missing %s: %v", want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(rootfs, "new/skip.txt")); err == nil {
		t.Error("ignored file copied")
	}
	if err := copyOp("/single", nil, "a.txt", "b.txt"); err == nil {
		t.Error("multiple sources without a trailing slash accepted")
	}
	if err := copyOp("/x", nil, "../outside"); err == nil {
		t.Error("missing source accepted")
	}
}

func TestResolveUser(t *testing.T) {
	rootfs := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootfs, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, "etc/passwd"), []byte("root:x:0:0::/root:/bin/sh\napp:x:1000:1001::/home/app:/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, "etc/group"), []byte("root:x:0:\nstaff:x:50:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for spec, want := range map[string][2]int{
		"":          {0, 0},
		"app":       {1000, 1001},
		"1000":      {1000, 1001},
		"app:staff": {1000, 50},
		"2000":      {2000, 2000},
		"2000:7":    {2000, 7},
	} {
		uid, gid, err := resolveUser(rootfs, spec)
		if err != nil || uid != want[0] || gid != want[1] {
			t.Errorf("resolveUser(%q) = %d:%d, %v; want %d:%d", spec, uid, gid, err, want[0], want[1])
		}
	}
	for _, bad := range []string{"nobody", "app:nogroup"} {
		if _, _, err := resolveUser(rootfs, bad); err == nil {
			t.Errorf("resolveUser(%q): no error", bad)
		}
	}
}

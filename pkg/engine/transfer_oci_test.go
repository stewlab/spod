// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// ImportOCI must preserve the source image's real layers and configuration:
// an image saved from another engine (podman save --format oci-archive)
// lands in the store behaving like the original, not as one opaque tar
// layer, and its rootfs is unpacked so containers can run from it.
func TestImportOCIPreservesImage(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()

	src := testImage(t, []imageEntry{
		{name: "usr/bin/tool", content: "#!/bin/sh\necho tool\n", mode: 0755},
		{name: "etc/tool.conf", content: "mode=on\n", mode: 0644},
	})
	cfg, err := src.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Config = v1.Config{
		Env:        []string{"PATH=/usr/local/bin:/usr/bin"},
		Entrypoint: []string{"/usr/bin/tool"},
		WorkingDir: "/work",
	}
	src, err = mutate.ConfigFile(src, cfg)
	if err != nil {
		t.Fatal(err)
	}

	archive := filepath.Join(t.TempDir(), "source.tar")
	out, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	if err := tarball.Write(nil, src, out); err != nil {
		out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}

	imported, err := e.ImportOCI(ctx, "tool:1.0", archive)
	if err != nil {
		t.Fatal(err)
	}
	if imported.Reference == "" {
		t.Fatal("imported image has no reference")
	}

	stored, err := e.InspectImage("tool:1.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Config.Entrypoint) != 1 || stored.Config.Entrypoint[0] != "/usr/bin/tool" {
		t.Fatalf("entrypoint lost in import: %+v", stored.Config)
	}
	if stored.Config.WorkingDir != "/work" {
		t.Fatalf("working dir lost in import: %+v", stored.Config)
	}
	for _, file := range []string{"usr/bin/tool", "etc/tool.conf"} {
		data, err := os.ReadFile(filepath.Join(stored.Rootfs, file))
		if err != nil {
			t.Fatalf("layer content missing at %s: %v", file, err)
		}
		if len(data) == 0 {
			t.Fatalf("layer content empty at %s", file)
		}
	}
}

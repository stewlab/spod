// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A container's filesystem must survive the export and import round trip,
// including changes made while the container ran, so the imported image can
// serve as a base for further containers.
func TestExportImportRoundTrip(t *testing.T) {
	ref, _ := publishTestImage(t, testImage(t, []imageEntry{{name: "usr/bin/tool", content: "base", mode: 0751}}))
	e := testEngine(t)
	ctx := context.Background()

	dest := filepath.Join(e.Root, "containers", "probe", "rootfs")
	if _, err := e.PrepareRootfs(ctx, ref, dest); err != nil {
		t.Fatal(err)
	}
	meta, err := json.Marshal(Container{Name: "probe", Image: ref, Status: statusCreated})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.metaPath("probe"), meta, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "marker.txt"), []byte("runtime-write"), 0644); err != nil {
		t.Fatal(err)
	}

	var archive bytes.Buffer
	if err := e.Export(ctx, "probe", &archive); err != nil {
		t.Fatalf("export: %v", err)
	}
	if err := e.Export(ctx, "absent", io.Discard); err == nil {
		t.Fatal("exported a container that does not exist")
	}

	const importedRef = "imported/base:latest"
	snapshot := archive.Bytes()
	imported, err := e.Import(ctx, importedRef, &archive)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	normalized, err := imageReference(importedRef)
	if err != nil {
		t.Fatal(err)
	}
	if imported.Reference != normalized.Name() || imported.Digest == "" {
		t.Fatalf("imported metadata: %+v", imported)
	}
	// Re-importing under the same reference replaces the stored image.
	if _, err := e.Import(ctx, importedRef, bytes.NewReader(snapshot)); err != nil {
		t.Fatalf("re-import: %v", err)
	}
	// A corrupt import must fail without destroying the stored image.
	if _, err := e.Import(ctx, importedRef, strings.NewReader("not a tar")); err == nil {
		t.Fatal("imported a corrupt archive")
	}
	images, err := e.Images()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, img := range images {
		if img.Reference == normalized.Name() {
			found = true
		}
	}
	if !found {
		t.Fatalf("imported image not listed: %+v", images)
	}

	prepared, err := e.PrepareRootfs(ctx, importedRef, filepath.Join(e.Root, "containers", "next", "rootfs"))
	if err != nil {
		t.Fatalf("prepare from imported image: %v", err)
	}
	assertFile(t, filepath.Join(prepared.Rootfs, "usr/bin/tool"), "base")
	assertFile(t, filepath.Join(prepared.Rootfs, "marker.txt"), "runtime-write")
}

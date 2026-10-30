// SPDX-License-Identifier: Apache-2.0
//go:build linux && cgo

package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRenameMovesContainerAndVolumes(t *testing.T) {
	ref, _ := publishTestImage(t, testImage(t, []imageEntry{{name: "usr/bin/tool", content: "x", mode: 0751}}))
	e := testEngine(t)
	ctx := context.Background()

	rootfs := filepath.Join(e.Root, "containers", "old-app", "rootfs")
	if _, err := e.PrepareRootfs(ctx, ref, rootfs); err != nil {
		t.Fatal(err)
	}
	meta := Container{
		Name: "old-app", Image: ref, Status: statusCreated,
		Options: RunOptions{
			Name: "old-app",
			Mounts: []Mount{
				{Source: filepath.Join(e.Root, "volumes", "old-app-home", "_data"), Target: "/home/dev"},
			},
		},
	}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.metaPath("old-app"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(e.Root, "volumes", "old-app-home", "_data"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.Root, "logs", "old-app.log"), []byte("log"), 0600); err != nil {
		t.Fatal(err)
	}
	// A container that has run keeps runtime metadata with its own copy of
	// the mounts; its volume sources must follow the rename too.
	runtime := runtimeMeta{
		Argv:     []string{"/bin/sleep", "3600"},
		Env:      []string{"PATH=/bin"},
		Cwd:      "/",
		Hostname: "old-app",
		Mounts: []Mount{
			{Source: filepath.Join(e.Root, "volumes", "old-app-home", "_data"), Target: "/home/dev"},
		},
	}
	if err := writeJSONAtomic(e.runtimePath("old-app"), runtime); err != nil {
		t.Fatal(err)
	}

	if err := e.Rename(ctx, "old-app", "new-app"); err != nil {
		t.Fatalf("rename: %v", err)
	}

	if _, err := os.Stat(e.containerDir("old-app")); !os.IsNotExist(err) {
		t.Fatalf("old container dir survived: %v", err)
	}
	renamed, err := e.Inspect(ctx, "new-app")
	if err != nil {
		t.Fatalf("inspect renamed container: %v", err)
	}
	if renamed.Name != "new-app" || renamed.Options.Name != "new-app" {
		t.Fatalf("renamed metadata: %+v", renamed)
	}
	wantSource := filepath.Join(e.Root, "volumes", "new-app-home", "_data")
	if len(renamed.Options.Mounts) != 1 || renamed.Options.Mounts[0].Source != wantSource {
		t.Fatalf("mount source not rewritten: %+v", renamed.Options.Mounts)
	}
	var rm runtimeMeta
	rmData, err := os.ReadFile(e.runtimePath("new-app"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rmData, &rm); err != nil {
		t.Fatal(err)
	}
	if rm.Hostname != "new-app" {
		t.Fatalf("runtime hostname not rewritten: %q", rm.Hostname)
	}
	if len(rm.Mounts) != 1 || rm.Mounts[0].Source != wantSource {
		t.Fatalf("runtime mount source not rewritten: %+v", rm.Mounts)
	}
	if _, err := os.Stat(filepath.Join(e.Root, "volumes", "old-app-home")); !os.IsNotExist(err) {
		t.Fatalf("old volume dir survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.Root, "logs", "new-app.log")); err != nil {
		t.Fatalf("log not renamed: %v", err)
	}

	// Name collisions and unknown containers fail without side effects.
	if err := e.Rename(ctx, "new-app", "new-app"); err != nil {
		t.Fatalf("rename to the same name must be a no-op: %v", err)
	}
	if _, err := os.Stat(e.containerDir("new-app")); err != nil {
		t.Fatalf("container missing after no-op rename: %v", err)
	}
	if err := e.Rename(ctx, "absent", "other"); err == nil {
		t.Fatal("renamed a container that does not exist")
	}
	if err := e.Rename(ctx, "new-app", "../escape"); err == nil {
		t.Fatal("accepted an invalid new name")
	}
}

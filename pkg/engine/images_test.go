// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

type imageEntry struct {
	name, content, link string
	kind                byte
	mode                int64
}

func imageTar(t *testing.T, entries ...imageEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	for _, entry := range entries {
		if entry.kind == 0 {
			entry.kind = tar.TypeReg
		}
		if entry.mode == 0 {
			entry.mode = 0644
		}
		header := &tar.Header{Name: entry.name, Typeflag: entry.kind, Mode: entry.mode, Linkname: entry.link, Uid: 23456, Gid: 23456}
		if entry.kind == tar.TypeReg {
			header.Size = int64(len(entry.content))
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(entry.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func testImage(t *testing.T, layers ...[]imageEntry) v1.Image {
	t.Helper()
	img := empty.Image
	for _, entries := range layers {
		layer, err := tarball.LayerFromReader(bytes.NewReader(imageTar(t, entries...)))
		if err != nil {
			t.Fatal(err)
		}
		img, err = mutate.AppendLayers(img, layer)
		if err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cfg.OS, cfg.Architecture = "linux", runtime.GOARCH
	cfg.Config = v1.Config{Env: []string{"HELLO=world"}, Entrypoint: []string{"/bin/sh"}, Cmd: []string{"-l"}, WorkingDir: "/work"}
	img, err = mutate.ConfigFile(img, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func publishTestImage(t *testing.T, img v1.Image) (string, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(server.Close)
	ref := strings.TrimPrefix(server.URL, "http://") + "/test/image:latest"
	tag, err := name.NewTag(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(tag, img); err != nil {
		t.Fatal(err)
	}
	return ref, server
}

func testEngine(t *testing.T) *Engine {
	t.Helper()
	e, err := New(imageTestDir(t))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func imageTestDir(t *testing.T) string {
	t.Helper()
	base, err := filepath.Abs(filepath.Join("..", "..", ".test-tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", base)
	return t.TempDir()
}

func TestImagesPullPrepareListRemove(t *testing.T) {
	base := []imageEntry{
		{name: "usr/bin/tool", content: "base", mode: 0751},
		{name: "gone", content: "removed"},
		{name: "opaque/old", content: "removed"},
		{name: "replace/old", content: "removed"},
		{name: "was-file", content: "removed"},
		{name: "bin", kind: tar.TypeSymlink, link: "/usr/bin"},
		{name: "dev/null", kind: tar.TypeChar},
	}
	top := []imageEntry{
		{name: "usr/bin/tool", content: "updated", mode: 0751},
		{name: ".wh.gone"},
		{name: "opaque/.wh..wh..opq"},
		{name: "opaque/new", content: "new"},
		{name: "replace", kind: tar.TypeSymlink, link: "/usr/bin"},
		{name: "was-file", kind: tar.TypeDir, mode: 0755},
		{name: "was-file/new", content: "new directory"},
	}
	ref, server := publishTestImage(t, testImage(t, base, top))
	e := testEngine(t)
	img, err := e.Pull(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if img.Digest == "" || img.Config.WorkingDir != "/work" || img.Platform.OS != "linux" {
		t.Fatalf("missing metadata: %+v", img)
	}
	for _, missing := range []string{"gone", "opaque/old", "dev/null", ".wh.gone", "opaque/.wh..wh..opq"} {
		if _, err := os.Lstat(filepath.Join(img.Rootfs, missing)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s survived: %v", missing, err)
		}
	}
	assertFile(t, filepath.Join(img.Rootfs, "usr/bin/tool"), "updated")
	assertFile(t, filepath.Join(img.Rootfs, "opaque/new"), "new")
	assertFile(t, filepath.Join(img.Rootfs, "was-file/new"), "new directory")
	if link, err := os.Readlink(filepath.Join(img.Rootfs, "replace")); err != nil || link != "/usr/bin" {
		t.Fatalf("layer replacement: %q, %v", link, err)
	}
	server.Close()
	if _, err := e.Pull(context.Background(), ref); err != nil {
		t.Fatalf("cached pull contacted registry: %v", err)
	}
	a, b := filepath.Join(e.Root, "containers", "a", "rootfs"), filepath.Join(e.Root, "containers", "b", "rootfs")
	for _, dest := range []string{a, b} {
		prepared, err := e.PrepareRootfs(context.Background(), ref, dest)
		if err != nil {
			t.Fatal(err)
		}
		if prepared.Rootfs != dest || prepared.Digest != img.Digest || prepared.Config.WorkingDir != "/work" {
			t.Fatalf("prepared metadata: %+v", prepared)
		}
		if link, err := os.Readlink(filepath.Join(dest, "bin")); err != nil || link != "/usr/bin" {
			t.Fatalf("absolute symlink lost: %q, %v", link, err)
		}
		info, err := os.Stat(filepath.Join(dest, "usr/bin/tool"))
		if err != nil || info.Mode().Perm() != 0751 {
			t.Fatalf("file mode lost: %v, %v", info, err)
		}
	}
	if err := os.WriteFile(filepath.Join(a, "usr/bin/tool"), []byte("changed"), 0751); err != nil {
		t.Fatal(err)
	}
	assertFile(t, filepath.Join(b, "usr/bin/tool"), "updated")
	assertFile(t, filepath.Join(img.Rootfs, "usr/bin/tool"), "updated")
	if _, err := e.PrepareRootfs(context.Background(), ref, a); err == nil {
		t.Fatal("overwrote a nonempty container rootfs")
	}
	images, err := e.Images()
	if err != nil || len(images) != 1 {
		t.Fatalf("list: %+v, %v", images, err)
	}
	if inspected, err := e.InspectImage(ref); err != nil || inspected.Digest != img.Digest {
		t.Fatalf("inspect: %+v, %v", inspected, err)
	}
	metadata, err := json.Marshal(Container{Name: "a", Image: ref})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(e.Root, "containers", "a", "container.json")
	if err := os.WriteFile(file, metadata, 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.RemoveImage(ref); err == nil {
		t.Fatal("removed an image used by a container")
	}
	if err := e.Discard(filepath.Dir(file)); err != nil {
		t.Fatal(err)
	}
	if err := e.RemoveImage(ref); err != nil {
		t.Fatal(err)
	}
	images, err = e.Images()
	if err != nil || len(images) != 0 {
		t.Fatalf("removed image still listed: %+v, %v", images, err)
	}
	assertFile(t, filepath.Join(b, "usr/bin/tool"), "updated")
	trash, err := os.ReadDir(filepath.Join(e.Root, "trash"))
	if err != nil || len(trash) < 2 {
		t.Fatalf("removed data was not retained: %v, %v", trash, err)
	}
}

func assertFile(t *testing.T, file, expected string) {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil || string(data) != expected {
		t.Fatalf("read %s: %q, %v; want %q", file, data, err, expected)
	}
}

func TestExtractRootfsRejectsEscapes(t *testing.T) {
	for name, entries := range map[string][]imageEntry{
		"dotdot":               {{name: "../outside", content: "bad"}},
		"absolute":             {{name: "/outside", content: "bad"}},
		"nested-dotdot":        {{name: "dir/../../outside", content: "bad"}},
		"backslash":            {{name: `dir\..\outside`, content: "bad"}},
		"symlink-parent":       {{name: "dir", kind: tar.TypeSymlink, link: "../outside"}, {name: "dir/file", content: "bad"}},
		"absolute-symlink":     {{name: "dir", kind: tar.TypeSymlink, link: "/"}, {name: "dir/file", content: "bad"}},
		"internal-symlink":     {{name: "dir", kind: tar.TypeSymlink, link: "other"}, {name: "dir/file", content: "bad"}},
		"symlink-replacement":  {{name: "file", kind: tar.TypeSymlink, link: "../outside"}, {name: "file", content: "bad"}},
		"hardlink-dotdot":      {{name: "file", kind: tar.TypeLink, link: "../outside"}},
		"hardlink-absolute":    {{name: "file", kind: tar.TypeLink, link: "/outside"}},
		"hardlink-symlink":     {{name: "sym", kind: tar.TypeSymlink, link: "../outside"}, {name: "file", kind: tar.TypeLink, link: "sym"}},
		"hardlink-parent-link": {{name: "dir", kind: tar.TypeSymlink, link: "../"}, {name: "file", kind: tar.TypeLink, link: "dir/outside"}},
		"hardlink-cycle":       {{name: "a", kind: tar.TypeLink, link: "b"}, {name: "b", kind: tar.TypeLink, link: "a"}},
	} {
		t.Run(name, func(t *testing.T) {
			base := imageTestDir(t)
			outside := filepath.Join(base, "outside")
			if err := os.WriteFile(outside, []byte("safe"), 0600); err != nil {
				t.Fatal(err)
			}
			dest := filepath.Join(base, "rootfs")
			if err := os.Mkdir(dest, 0700); err != nil {
				t.Fatal(err)
			}
			if err := extractRootfs(context.Background(), dest, bytes.NewReader(imageTar(t, entries...))); err == nil {
				t.Fatal("accepted unsafe archive")
			}
			assertFile(t, outside, "safe")
		})
	}
}

func TestExtractRootfsModesLinksAndDevices(t *testing.T) {
	dest := imageTestDir(t)
	data := imageTar(t,
		imageEntry{name: "readonly", kind: tar.TypeDir, mode: 0555},
		imageEntry{name: "readonly/link", kind: tar.TypeLink, link: "readonly/target"},
		imageEntry{name: "readonly/target", content: "hello", mode: 0751},
		imageEntry{name: "absolute", kind: tar.TypeSymlink, link: "/readonly/target"},
		imageEntry{name: "relative", kind: tar.TypeSymlink, link: "readonly/target"},
		imageEntry{name: "broken", kind: tar.TypeSymlink, link: "/not-present"},
		imageEntry{name: "dev/null", kind: tar.TypeChar},
		imageEntry{name: "fifo", kind: tar.TypeFifo},
	)
	if err := extractRootfs(context.Background(), dest, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dest, "readonly"), 0755) })
	a, err := os.Stat(filepath.Join(dest, "readonly/link"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(filepath.Join(dest, "readonly/target"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(a, b) {
		t.Fatal("hardlink was not preserved")
	}
	copyDest := imageTestDir(t)
	if err := copyRootfs(context.Background(), dest, copyDest); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(copyDest, "readonly"), 0755) })
	copyA, err := os.Stat(filepath.Join(copyDest, "readonly/link"))
	if err != nil {
		t.Fatal(err)
	}
	copyB, err := os.Stat(filepath.Join(copyDest, "readonly/target"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "linux" && !os.SameFile(copyA, copyB) {
		t.Fatal("hardlink was not preserved within the writable copy")
	}
	if os.SameFile(a, copyA) {
		t.Fatal("writable copy shares hardlinks with the cache")
	}
	dir, err := os.Stat(filepath.Join(dest, "readonly"))
	if err != nil || dir.Mode().Perm() != 0555 {
		t.Fatalf("directory mode lost: %v, %v", dir, err)
	}
	for _, file := range []string{"dev/null", "fifo"} {
		if _, err := os.Lstat(filepath.Join(dest, file)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("special file %s was extracted: %v", file, err)
		}
	}
	for link, target := range map[string]string{"absolute": "/readonly/target", "relative": "readonly/target", "broken": "/not-present"} {
		if got, err := os.Readlink(filepath.Join(dest, link)); err != nil || got != target {
			t.Fatalf("symlink %s: %q, %v", link, got, err)
		}
	}
}

func TestImagesConcurrentPull(t *testing.T) {
	ref, _ := publishTestImage(t, testImage(t, []imageEntry{{name: "file", content: "payload"}}))
	e := testEngine(t)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			other, err := New(e.Root)
			if err == nil {
				_, err = other.Pull(context.Background(), ref)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	images, err := e.Images()
	if err != nil || len(images) != 1 {
		t.Fatalf("concurrent publication: %+v, %v", images, err)
	}
}

func TestImagesRemovalProtectsStoppedContainerMetadata(t *testing.T) {
	ref, _ := publishTestImage(t, testImage(t, []imageEntry{{name: "file", content: "payload"}}))
	e := testEngine(t)
	img, err := e.Pull(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.Root, "containers", "stopped")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, ignored := range []string{"rootfs", "state"} {
		subdir := filepath.Join(dir, ignored)
		if err := os.Mkdir(subdir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(subdir, "container.json"), []byte("not container metadata"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, container := range []Container{
		{Name: "stopped", Status: "stopped", Image: ref},
		{Name: "stopped", Status: "stopped", Options: RunOptions{Image: ref}},
		{Name: "stopped", Status: "stopped", Image: strings.Split(ref, ":latest")[0] + "@" + img.Digest},
	} {
		data, err := json.Marshal(container)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "container.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
		if err := e.RemoveImage(ref); err == nil || !strings.Contains(err.Error(), "used by container stopped") {
			t.Fatalf("removed image retained by stopped container %+v: %v", container, err)
		}
	}
	if err := e.Discard(dir); err != nil {
		t.Fatal(err)
	}
	if err := e.RemoveImage(ref); err != nil {
		t.Fatal(err)
	}
}

func TestImagesSelectLinuxPlatform(t *testing.T) {
	linux := testImage(t, []imageEntry{{name: "platform", content: "linux"}})
	windows := testImage(t, []imageEntry{{name: "platform", content: "windows"}})
	cfg, err := windows.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cfg.OS = "windows"
	windows, err = mutate.ConfigFile(windows, cfg)
	if err != nil {
		t.Fatal(err)
	}
	index := mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{Add: windows, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "windows", Architecture: runtime.GOARCH}}},
		mutate.IndexAddendum{Add: linux, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: runtime.GOARCH}}},
	)
	server := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	defer server.Close()
	ref := strings.TrimPrefix(server.URL, "http://") + "/multi:latest"
	tag, err := name.NewTag(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(tag, index); err != nil {
		t.Fatal(err)
	}
	img, err := testEngine(t).Pull(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	assertFile(t, filepath.Join(img.Rootfs, "platform"), "linux")
}

func TestImagesProcessPullHelper(t *testing.T) {
	root, ref := os.Getenv("SPOD_TEST_PULL_ROOT"), os.Getenv("SPOD_TEST_PULL_REF")
	if root == "" || ref == "" {
		t.Skip("subprocess helper")
	}
	e, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Pull(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
}

func TestImagesConcurrentProcesses(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("cross-process locking is Linux-only")
	}
	ref, _ := publishTestImage(t, testImage(t, []imageEntry{{name: "file", content: "payload"}}))
	e := testEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	type child struct {
		command *exec.Cmd
		output  bytes.Buffer
	}
	children := make([]*child, 3)
	for i := range children {
		c := &child{command: exec.CommandContext(ctx, os.Args[0], "-test.run=^TestImagesProcessPullHelper$")}
		c.command.Env = append(os.Environ(), "SPOD_TEST_PULL_ROOT="+e.Root, "SPOD_TEST_PULL_REF="+ref)
		c.command.Stdout, c.command.Stderr = &c.output, &c.output
		if err := c.command.Start(); err != nil {
			t.Fatal(err)
		}
		children[i] = c
	}
	for _, c := range children {
		if err := c.command.Wait(); err != nil {
			t.Fatalf("concurrent child: %v\n%s", err, c.output.String())
		}
	}
	images, err := e.Images()
	if err != nil || len(images) != 1 {
		t.Fatalf("concurrent processes: %+v, %v", images, err)
	}
	assertFile(t, filepath.Join(images[0].Rootfs, "file"), "payload")
}

func TestImagesProcessLockCancellation(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("cross-process locking is Linux-only")
	}
	e := testEngine(t)
	unlock, err := lockImageProcesses(context.Background(), filepath.Join(e.Root, "images", ".lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := e.Pull(ctx, "example.invalid/repo:tag"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("process lock ignored cancellation: %v", err)
	}
}

func TestImagesFailedPullIsNotPublished(t *testing.T) {
	img := testImage(t, []imageEntry{{name: "file", content: "payload"}})
	backend := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	var broken atomic.Bool
	layers, err := img.Layers()
	if err != nil {
		t.Fatal(err)
	}
	layerHash, err := layers[0].Digest()
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if broken.Load() && r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/blobs/"+layerHash.String()) {
			w.Header().Set("Content-Length", "200")
			_, _ = w.Write([]byte("truncated"))
			return
		}
		backend.ServeHTTP(w, r)
	}))
	defer server.Close()
	ref := strings.TrimPrefix(server.URL, "http://") + "/broken:latest"
	tag, err := name.NewTag(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(tag, img); err != nil {
		t.Fatal(err)
	}
	broken.Store(true)
	e := testEngine(t)
	if _, err := e.Pull(context.Background(), ref); err == nil {
		t.Fatal("accepted incomplete image")
	}
	if _, err := e.InspectImage(ref); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed pull published: %v", err)
	}
	images, err := e.Images()
	if err != nil || len(images) != 0 {
		t.Fatalf("failed pull listed: %+v, %v", images, err)
	}
	broken.Store(false)
	if _, err := e.Pull(context.Background(), ref); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
}

func TestImagesCanceledPullAndHashedReferences(t *testing.T) {
	e := testEngine(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Pull(ctx, "example.invalid/repo:tag"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled pull: %v", err)
	}
	if _, err := e.InspectImage("../../outside"); err == nil {
		t.Fatal("accepted invalid reference")
	}
	a, _ := imageReference("alpine")
	b, _ := imageReference("docker.io/library/alpine:latest")
	if e.imageDir(a) != e.imageDir(b) || len(filepath.Base(e.imageDir(a))) != 64 {
		t.Fatal("references are not canonicalized and hashed")
	}
}

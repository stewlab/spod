// SPDX-License-Identifier: Apache-2.0
//go:build linux && cgo

// Container store: the on-disk state of named containers — metadata, libcontainer
// state directories, named volumes, logs, and attach sockets — plus the
// operations that inspect or mutate it (Inspect, List, Rename, Remove). The
// process lifecycle (Run, Start, Exec, Stop) lives in runtime_linux.go.

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/opencontainers/runc/libcontainer"
	"golang.org/x/sys/unix"
)

const (
	statusCreated = "created"
	statusRunning = "running"
	statusExited  = "exited"
)

// runtimeMeta is the engine-private, fully resolved execution spec persisted
// alongside the shared Container metadata. It lets Start re-launch a stopped
// container even if its source image has since been removed.
type runtimeMeta struct {
	Argv     []string          `json:"argv"`
	Env      []string          `json:"env"`
	Cwd      string            `json:"cwd"`
	UID      int               `json:"uid"`
	GID      int               `json:"gid"`
	Hostname string            `json:"hostname"`
	Mounts   []Mount           `json:"mounts"`
	TTY      bool              `json:"tty"`
	Labels   map[string]string `json:"labels,omitempty"`
}

// perContainerLocks guards create/state mutations within this process; a flock
// on the container directory guards across processes.
var perContainerLocks sync.Map

func (e *Engine) containerLock(name string) *sync.Mutex {
	m, _ := perContainerLocks.LoadOrStore(e.Root+"\x00"+name, &sync.Mutex{})
	return m.(*sync.Mutex)
}

func (e *Engine) containerDir(name string) string { return filepath.Join(e.Root, "containers", name) }
func (e *Engine) rootfsDir(name string) string    { return filepath.Join(e.containerDir(name), "rootfs") }
func (e *Engine) stateDir(name string) string     { return filepath.Join(e.containerDir(name), "state") }
func (e *Engine) metaPath(name string) string {
	return filepath.Join(e.containerDir(name), "container.json")
}
func (e *Engine) runtimePath(name string) string {
	return filepath.Join(e.containerDir(name), "runtime.json")
}

// lockContainer takes the in-process mutex and an exclusive flock on the
// container directory, serializing concurrent create/state mutations.
func (e *Engine) lockContainer(ctx context.Context, name string) (func(), error) {
	mu := e.containerLock(name)
	mu.Lock()
	if err := os.MkdirAll(e.containerDir(name), 0700); err != nil {
		mu.Unlock()
		return nil, err
	}
	unlockFile, err := lockImageProcesses(ctx, filepath.Join(e.containerDir(name), ".lock"))
	if err != nil {
		mu.Unlock()
		return nil, err
	}
	return func() {
		unlockFile()
		mu.Unlock()
	}, nil
}

func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (e *Engine) loadContainer(name string) (*Container, error) {
	data, err := os.ReadFile(e.metaPath(name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no such container: %s", name)
		}
		return nil, err
	}
	var c Container
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("corrupt metadata for %s: %w", name, err)
	}
	return &c, nil
}

func (e *Engine) loadRuntimeMeta(name string) (*runtimeMeta, error) {
	data, err := os.ReadFile(e.runtimePath(name))
	if err != nil {
		return nil, err
	}
	var m runtimeMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// refreshStatus reconciles persisted metadata with the live libcontainer state.
func (e *Engine) refreshStatus(c *Container) {
	cont, err := libcontainer.Load(e.stateDir(c.Name), c.Name)
	if err != nil {
		if c.Status == statusRunning {
			c.Status = statusExited
			c.PID = 0
		}
		return
	}
	st, err := cont.Status()
	if err != nil {
		return
	}
	switch st {
	case libcontainer.Running, libcontainer.Created, libcontainer.Paused:
		c.Status = statusRunning
		if pids, err := cont.Processes(); err == nil && len(pids) > 0 {
			c.PID = pids[0]
		}
	default:
		c.Status = statusExited
		c.PID = 0
	}
}

// Rename changes a stopped container's name. Its store directories, metadata,
// named volumes, attach socket, and logs move with it, and mount sources
// referencing the moved volumes are rewritten. Running containers must be
// stopped first. Every move is journaled with its inverse, and metadata is
// replaced atomically, so a failure restores the original state instead of
// stranding data.
func (e *Engine) Rename(ctx context.Context, oldName, newName string) error {
	if err := ValidateName(oldName); err != nil {
		return fmt.Errorf("old name: %w", err)
	}
	if err := ValidateName(newName); err != nil {
		return fmt.Errorf("new name: %w", err)
	}
	if oldName == newName {
		return nil
	}
	unlock, err := e.lockContainer(ctx, oldName)
	if err != nil {
		return err
	}
	defer unlock()

	meta, err := e.loadContainer(oldName)
	if err != nil {
		return err
	}
	e.refreshStatus(meta)
	if meta.Status != statusCreated && meta.Status != statusExited {
		return fmt.Errorf("container %s is %s; stop it before renaming", oldName, meta.Status)
	}
	if _, err := os.Stat(e.metaPath(newName)); err == nil {
		return fmt.Errorf("container name already in use: %s", newName)
	}
	// runtime.json is optional for containers that were created but never ran.
	rmJSON, hasRuntime, rmErr := readOptionalFile(e.runtimePath(oldName))
	if rmErr != nil {
		return rmErr
	}

	volumesDir := filepath.Join(e.Root, "volumes")
	entries, err := os.ReadDir(volumesDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	type move struct{ do, undo func() error }
	var moves []move
	var movedVolumes []struct{ from, to string }
	apply := func() error {
		for _, m := range moves {
			if err := m.do(); err != nil {
				return err
			}
		}
		return nil
	}
	rollback := func() {
		for i := len(moves) - 1; i >= 0; i-- {
			_ = moves[i].undo()
		}
	}
	add := func(do, undo func() error) {
		moves = append(moves, move{do, undo})
	}

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, oldName+"-") {
			continue
		}
		from := filepath.Join(volumesDir, name)
		to := filepath.Join(volumesDir, newName+strings.TrimPrefix(name, oldName))
		add(
			func() error { return os.Rename(from, to) },
			func() error { return os.Rename(to, from) },
		)
		movedVolumes = append(movedVolumes, struct{ from, to string }{from, to})
	}
	add(
		func() error { return os.Rename(e.containerDir(oldName), e.containerDir(newName)) },
		func() error { return os.Rename(e.containerDir(newName), e.containerDir(oldName)) },
	)
	oldLog := filepath.Join(e.Root, "logs", oldName+".log")
	newLog := filepath.Join(e.Root, "logs", newName+".log")
	if _, err := os.Lstat(oldLog); err == nil {
		add(
			func() error { return os.Rename(oldLog, newLog) },
			func() error { return os.Rename(newLog, oldLog) },
		)
	}
	oldSocket := filepath.Join(e.Root, "run", oldName+".attach")
	newSocket := filepath.Join(e.Root, "run", newName+".attach")
	if _, err := os.Lstat(oldSocket); err == nil {
		add(
			func() error { return os.Rename(oldSocket, newSocket) },
			func() error { return os.Rename(newSocket, oldSocket) },
		)
	}

	if err := apply(); err != nil {
		rollback()
		return fmt.Errorf("rename container %s: %w", oldName, err)
	}

	rewriteVolumes := func(mounts []Mount) {
		for i, m := range mounts {
			for _, v := range movedVolumes {
				if m.Source == v.from || strings.HasPrefix(m.Source, v.from+string(filepath.Separator)) {
					mounts[i].Source = v.to + strings.TrimPrefix(m.Source, v.from)
					break
				}
			}
		}
	}
	rewriteVolumes(meta.Options.Mounts)
	meta.Name = newName
	meta.Options.Name = newName
	if err := writeJSONAtomic(e.metaPath(newName), meta); err != nil {
		rollback()
		return fmt.Errorf("rename container %s: %w", oldName, err)
	}
	if hasRuntime {
		var rm runtimeMeta
		if err := json.Unmarshal(rmJSON, &rm); err != nil {
			rollback()
			return fmt.Errorf("rename container %s: %w", oldName, err)
		}
		if rm.Hostname == oldName {
			rm.Hostname = newName
			if err := writeJSONAtomic(e.runtimePath(newName), rm); err != nil {
				rollback()
				return fmt.Errorf("rename container %s: %w", oldName, err)
			}
		}
	}
	return nil
}

// readOptionalFile returns the contents of a file that may not exist; a
// missing file yields nil contents with hasRuntime false and no error.
func readOptionalFile(path string) ([]byte, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return data, true, nil
}

// Remove deletes a container's metadata and rootfs. A running container is only
// removed when force is true (it is killed first). Removed resources are moved
// to the store trash, never deleted outright.
func (e *Engine) Remove(ctx context.Context, name string, force bool) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	unlock, err := e.lockContainer(ctx, name)
	if err != nil {
		return err
	}
	defer unlock()

	if _, err := os.Stat(e.metaPath(name)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no such container: %s", name)
		}
		return err
	}
	if cont, err := libcontainer.Load(e.stateDir(name), name); err == nil {
		st, _ := cont.Status()
		if st != libcontainer.Stopped {
			if !force {
				return fmt.Errorf("container is running (use force to remove): %s", name)
			}
			_ = cont.Signal(unix.SIGKILL)
			for i := 0; i < 80; i++ {
				if s, err := cont.Status(); err != nil || s == libcontainer.Stopped {
					break
				}
				time.Sleep(25 * time.Millisecond)
			}
		}
		_ = cont.Destroy()
	}
	// Drop the attach socket with the container: a leftover would make the
	// next run of the same name wait out a stale bind.
	_ = e.Discard(filepath.Join(e.Root, "run", name+".attach"))
	return e.Discard(e.containerDir(name))
}

// Inspect returns the current metadata for a container, reconciled with its
// live runtime state.
func (e *Engine) Inspect(ctx context.Context, name string) (*Container, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	meta, err := e.loadContainer(name)
	if err != nil {
		return nil, err
	}
	e.refreshStatus(meta)
	return meta, nil
}

// List returns all containers in the store with reconciled status.
func (e *Engine) List(ctx context.Context) ([]Container, error) {
	entries, err := os.ReadDir(filepath.Join(e.Root, "containers"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []Container
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		meta, err := e.loadContainer(ent.Name())
		if err != nil {
			continue
		}
		e.refreshStatus(meta)
		out = append(out, *meta)
	}
	return out, nil
}

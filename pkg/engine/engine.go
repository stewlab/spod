// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

var ErrUnsupported = errors.New("spod container execution requires Linux user namespaces")

// Engine is a daemonless, per-user image and container store.
type Engine struct {
	Root string
}

func DefaultRoot() (string, error) {
	if root := os.Getenv("SPOD_ROOT"); root != "" {
		return filepath.Abs(root)
	}
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Abs(filepath.Join(base, "spod", "storage"))
}

func New(root string) (*Engine, error) {
	var err error
	if root == "" {
		root, err = DefaultRoot()
	} else {
		root, err = filepath.Abs(root)
	}
	if err != nil {
		return nil, err
	}
	for _, sub := range []string{"images", "containers", "volumes", "run", "logs", "trash"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0700); err != nil {
			return nil, err
		}
	}
	return &Engine{Root: root}, nil
}

var validName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

func ValidateName(name string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("invalid name %q: use 1-128 letters, digits, dots, underscores or hyphens", name)
	}
	return nil
}

// Discard retains removed resources in the store's trash rather than deleting them.
func (e *Engine) Discard(path string) error {
	rel, err := filepath.Rel(e.Root, path)
	if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) || len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator) {
		return fmt.Errorf("refusing to discard path outside storage: %s", path)
	}
	return os.Rename(path, filepath.Join(e.Root, "trash", fmt.Sprintf("%d-%s", time.Now().UnixNano(), filepath.Base(path))))
}

type Mount struct {
	Source   string
	Target   string
	ReadOnly bool
}

type RunOptions struct {
	Name        string
	Image       string
	Command     []string
	Entrypoint  []string
	Env         []string
	WorkDir     string
	User        string
	Hostname    string
	Mounts      []Mount
	Labels      map[string]string
	TTY         bool
	Interactive bool
	Remove      bool
	Network     string
}

type ExecOptions struct {
	Command []string
	Env     []string
	WorkDir string
	User    string
	TTY     bool
}

type IO struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

type Container struct {
	Name       string
	Image      string
	Status     string
	Entrypoint []string
	PID        int
	Created    time.Time
	Options    RunOptions
}

// Lifecycle is implemented directly by Engine on Linux and returns ErrUnsupported elsewhere.
type Lifecycle interface {
	Run(context.Context, RunOptions, IO) (int, error)
	Start(context.Context, string, IO) (int, error)
	Exec(context.Context, string, ExecOptions, IO) (int, error)
	Stop(context.Context, string, time.Duration) error
	Remove(context.Context, string, bool) error
	Inspect(context.Context, string) (*Container, error)
	List(context.Context) ([]Container, error)
}

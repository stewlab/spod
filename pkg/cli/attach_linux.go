// SPDX-License-Identifier: Apache-2.0
//go:build linux

package cli

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/stewlab/spod/pkg/engine"
)

type outputHub struct {
	mu      sync.Mutex
	clients map[net.Conn]struct{}
}

func (h *outputHub) Write(data []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for conn := range h.clients {
		_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
		if _, err := conn.Write(data); err != nil {
			_ = conn.Close()
			delete(h.clients, conn)
		}
	}
	return len(data), nil
}

// /proc/self/fd avoids Unix socket pathname limits with long storage paths.
func attachAddress(e *engine.Engine, name string) (string, *os.File, error) {
	if err := engine.ValidateName(name); err != nil {
		return "", nil, err
	}
	dir, err := os.Open(filepath.Join(e.Root, "run"))
	if err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("/proc/self/fd/%d/%s.attach", dir.Fd(), name), dir, nil
}

func monitorStreams(cmd *cobra.Command, e *engine.Engine, name string, interactive bool) (engine.IO, func(), error) {
	address, dir, err := attachAddress(e, name)
	if err != nil {
		return engine.IO{}, nil, err
	}
	defer dir.Close()
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: address, Net: "unix"})
	if err != nil {
		if conn, dialErr := net.DialTimeout("unix", address, 200*time.Millisecond); dialErr == nil {
			_ = conn.Close()
		} else if info, statErr := os.Lstat(filepath.Join(e.Root, "run", name+".attach")); statErr == nil && info.Mode()&os.ModeSocket != 0 {
			if discardErr := e.Discard(filepath.Join(e.Root, "run", name+".attach")); discardErr == nil {
				listener, err = net.ListenUnix("unix", &net.UnixAddr{Name: address, Net: "unix"})
			}
		}
	}
	if err != nil {
		return engine.IO{}, nil, fmt.Errorf("creating attach socket: %w", err)
	}
	listener.SetUnlinkOnClose(false)
	socketPath := filepath.Join(e.Root, "run", name+".attach")
	if err := os.Chmod(socketPath, 0600); err != nil {
		_ = listener.Close()
		_ = e.Discard(socketPath)
		return engine.IO{}, nil, err
	}
	hub := &outputHub{clients: make(map[net.Conn]struct{})}
	reader, writer, err := os.Pipe()
	if err != nil {
		_ = listener.Close()
		_ = e.Discard(socketPath)
		return engine.IO{}, nil, err
	}
	var input io.Reader
	if interactive {
		input = reader
	}
	closed := false
	go func() {
		for {
			conn, err := listener.AcceptUnix()
			if err != nil {
				return
			}
			hub.mu.Lock()
			if closed {
				hub.mu.Unlock()
				_ = conn.Close()
				return
			}
			hub.clients[conn] = struct{}{}
			hub.mu.Unlock()
			go func() {
				if interactive {
					_, _ = io.Copy(writer, conn)
				} else {
					_, _ = io.Copy(io.Discard, conn)
				}
				// A peer may close its write half while continuing to read output.
			}()
		}
	}()
	closeIO := func() {
		_ = listener.Close()
		_ = reader.Close()
		_ = writer.Close()
		hub.mu.Lock()
		closed = true
		for conn := range hub.clients {
			_ = conn.Close()
		}
		hub.clients = nil
		hub.mu.Unlock()
		_ = e.Discard(socketPath)
	}
	return engine.IO{
		Stdin: input, Stdout: io.MultiWriter(cmd.OutOrStdout(), hub),
		Stderr: io.MultiWriter(cmd.ErrOrStderr(), hub),
	}, closeIO, nil
}

func addAttach(app *cobra.Command, withStore storeHandler) {
	app.AddCommand(&cobra.Command{
		Use: "attach NAME", Args: cobra.ExactArgs(1),
		Short: "Attach to a detached container's live output and stdin (when started with -i)",
		RunE: withStore(func(cmd *cobra.Command, e *engine.Engine, args []string) error {
			container, err := e.Inspect(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if container.Status != "running" {
				return fmt.Errorf("container %s is not running", args[0])
			}
			address, dir, err := attachAddress(e, args[0])
			if err != nil {
				return err
			}
			defer dir.Close()
			conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: address, Net: "unix"})
			if err != nil {
				return fmt.Errorf("container has no detached monitor to attach to: %w", err)
			}
			defer conn.Close()
			if container.Options.Interactive {
				go func() {
					_, _ = io.Copy(conn, cmd.InOrStdin())
					_ = conn.CloseWrite()
				}()
			}
			done := make(chan error, 1)
			go func() {
				_, err := io.Copy(cmd.OutOrStdout(), conn)
				done <- err
			}()
			select {
			case err := <-done:
				return err
			case <-cmd.Context().Done():
				return cmd.Context().Err()
			}
		}),
	})
}

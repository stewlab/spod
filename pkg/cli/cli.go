// SPDX-License-Identifier: Apache-2.0

// Package cli provides the same commands to the standalone spod binary and embedders.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/spf13/cobra"
	"github.com/stewlab/spod/pkg/engine"
)

type exitError int

func (e exitError) Error() string { return fmt.Sprintf("container exited with status %d", e) }

func ExitCode(err error) int {
	var code exitError
	if errors.As(err, &code) {
		return int(code)
	}
	return 1
}

func result(code int, err error) error {
	if err != nil {
		return err
	}
	if code != 0 {
		return exitError(code)
	}
	return nil
}

func streams(cmd *cobra.Command) engine.IO {
	return engine.IO{Stdin: cmd.InOrStdin(), Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr()}
}

func NewCommand() *cobra.Command {
	var root string
	app := &cobra.Command{
		Use: "spod", Short: "Standalone, rootless, daemonless OCI containers",
		Version: "0.1.0", SilenceUsage: true, SilenceErrors: true,
	}
	app.PersistentFlags().StringVar(&root, "root", "", "Storage directory (default: SPOD_ROOT or XDG_DATA_HOME/spod/storage)")
	store := func() (*engine.Engine, error) { return engine.New(root) }
	withStore := func(fn func(*cobra.Command, *engine.Engine, []string) error) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error {
			e, err := store()
			if err != nil {
				return err
			}
			return fn(cmd, e, args)
		}
	}

	var opts engine.RunOptions
	var detach bool
	var volumes, devices, envs, labels []string
	var entrypoint string
	run := &cobra.Command{
		Use: "run [options] IMAGE [COMMAND [ARG...]]", Args: cobra.MinimumNArgs(1),
		Short: "Create and run a container",
		RunE: withStore(func(cmd *cobra.Command, e *engine.Engine, args []string) error {
			opts.Image = args[0]
			opts.Command = args[1:]
			opts.Env = envs
			if opts.Name == "" {
				opts.Name = fmt.Sprintf("spod-%d", time.Now().UnixNano())
			}
			if err := engine.ValidateName(opts.Name); err != nil {
				return err
			}
			if entrypoint != "" {
				opts.Entrypoint = []string{entrypoint}
			} else if cmd.Flags().Changed("entrypoint") {
				opts.Entrypoint = []string{}
			}
			opts.Labels = make(map[string]string)
			for _, label := range labels {
				key, value, _ := strings.Cut(label, "=")
				opts.Labels[key] = value
			}
			for _, spec := range volumes {
				mount, err := parseMount(e, spec)
				if err != nil {
					return err
				}
				opts.Mounts = append(opts.Mounts, mount)
			}
			for _, spec := range devices {
				if strings.Contains(spec, "=") {
					return fmt.Errorf("CDI devices are not supported by spod: %s; use a host device path or Podman", spec)
				}
				if !strings.HasPrefix(spec, "/") {
					return fmt.Errorf("device requires an absolute host path: %s", spec)
				}
				mount, err := parseMount(e, spec)
				if err != nil {
					return err
				}
				opts.Mounts = append(opts.Mounts, mount)
			}
			if opts.Network != "host" {
				return fmt.Errorf("spod supports --network=host only; use Podman or Docker for isolated networking")
			}
			if detach {
				if opts.TTY {
					return fmt.Errorf("detached containers do not support -t; use exec -it after starting")
				}
				return launch(cmd, e, "run", opts.Name, &opts)
			}
			stdio := streams(cmd)
			if !opts.Interactive {
				stdio.Stdin = nil
			}
			code, err := e.Run(cmd.Context(), opts, stdio)
			return result(code, err)
		}),
	}
	f := run.Flags()
	f.SetInterspersed(false)
	f.StringVar(&opts.Name, "name", "", "Container name")
	f.StringVar(&opts.Hostname, "hostname", "", "Container hostname")
	f.BoolVarP(&detach, "detach", "d", false, "Run in background without a daemon")
	f.BoolVarP(&opts.Interactive, "interactive", "i", false, "Keep stdin open")
	f.BoolVarP(&opts.TTY, "tty", "t", false, "Allocate a pseudo-terminal")
	f.BoolVar(&opts.Remove, "rm", false, "Remove container after it exits")
	f.StringVarP(&opts.WorkDir, "workdir", "w", "", "Working directory inside container")
	f.StringVarP(&opts.User, "user", "u", "", "Numeric container UID[:GID]")
	f.StringVar(&opts.Network, "network", "host", "Network mode (host only)")
	f.StringVar(&entrypoint, "entrypoint", "", "Override image entrypoint")
	f.StringArrayVarP(&envs, "env", "e", nil, "Environment KEY=VALUE")
	f.StringArrayVarP(&volumes, "volume", "v", nil, "Bind or named volume SOURCE:TARGET[:ro]")
	f.StringArrayVar(&devices, "device", nil, "Bind host device PATH[:TARGET]")
	f.StringArrayVar(&labels, "label", nil, "Container metadata KEY=VALUE")
	app.AddCommand(run)

	start := &cobra.Command{
		Use: "start NAME", Args: cobra.ExactArgs(1), Short: "Restart a stopped container in the background",
		RunE: withStore(func(cmd *cobra.Command, e *engine.Engine, args []string) error {
			return launch(cmd, e, "start", args[0], nil)
		}),
	}
	app.AddCommand(start)
	var timeout time.Duration
	stop := &cobra.Command{
		Use: "stop NAME", Args: cobra.ExactArgs(1), Short: "Stop a container with SIGTERM, then SIGKILL after timeout",
		RunE: withStore(func(cmd *cobra.Command, e *engine.Engine, args []string) error {
			return e.Stop(cmd.Context(), args[0], timeout)
		}),
	}
	stop.Flags().DurationVar(&timeout, "timeout", 10*time.Second, "Grace period")
	app.AddCommand(stop)

	var execOpts engine.ExecOptions
	var interactive bool
	execCmd := &cobra.Command{
		Use: "exec [options] NAME COMMAND [ARG...]", Args: cobra.MinimumNArgs(2), Short: "Run a process in a running container",
		RunE: withStore(func(cmd *cobra.Command, e *engine.Engine, args []string) error {
			execOpts.Command = args[1:]
			stdio := streams(cmd)
			if !interactive {
				stdio.Stdin = nil
			}
			code, err := e.Exec(cmd.Context(), args[0], execOpts, stdio)
			return result(code, err)
		}),
	}
	execCmd.Flags().SetInterspersed(false)
	execCmd.Flags().BoolVarP(&execOpts.TTY, "tty", "t", false, "Allocate a pseudo-terminal")
	execCmd.Flags().BoolVarP(&interactive, "interactive", "i", false, "Read stdin")
	execCmd.Flags().StringVarP(&execOpts.User, "user", "u", "", "Numeric container UID[:GID]")
	execCmd.Flags().StringVarP(&execOpts.WorkDir, "workdir", "w", "", "Working directory")
	execCmd.Flags().StringArrayVarP(&execOpts.Env, "env", "e", nil, "Environment KEY=VALUE")
	app.AddCommand(execCmd)

	var force bool
	rm := &cobra.Command{
		Use: "rm NAME", Args: cobra.ExactArgs(1), Short: "Remove a container (retained in storage trash)",
		RunE: withStore(func(cmd *cobra.Command, e *engine.Engine, args []string) error {
			return e.Remove(cmd.Context(), args[0], force)
		}),
	}
	rm.Flags().BoolVarP(&force, "force", "f", false, "Stop container first")
	app.AddCommand(rm)
	ps := &cobra.Command{
		Use: "ps", Args: cobra.NoArgs, Short: "List containers",
		RunE: withStore(func(cmd *cobra.Command, e *engine.Engine, _ []string) error {
			list, err := e.List(cmd.Context())
			if err != nil {
				return err
			}
			if list == nil {
				list = []engine.Container{}
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(list)
		}),
	}
	app.AddCommand(ps)
	pull := &cobra.Command{
		Use: "pull IMAGE", Args: cobra.ExactArgs(1), Short: "Fetch and unpack an OCI image without a daemon",
		RunE: withStore(func(cmd *cobra.Command, e *engine.Engine, args []string) error {
			image, err := e.Pull(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), image.Reference)
			return nil
		}),
	}
	app.AddCommand(pull)
	app.AddCommand(&cobra.Command{
		Use: "images", Args: cobra.NoArgs, Short: "List stored images",
		RunE: withStore(func(cmd *cobra.Command, e *engine.Engine, _ []string) error {
			images, err := e.Images()
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(images)
		}),
	})
	rmi := &cobra.Command{
		Use: "rmi IMAGE", Args: cobra.ExactArgs(1), Short: "Remove a stored image (retained in storage trash)",
		RunE: withStore(func(cmd *cobra.Command, e *engine.Engine, args []string) error {
			return e.RemoveImage(args[0])
		}),
	}
	rmi.Flags().Bool("force", false, "Accepted for compatibility; images in use are never removed")
	rmi.Flags().Lookup("force").Shorthand = "f"
	app.AddCommand(rmi)
	var format, kind string
	inspect := &cobra.Command{
		Use: "inspect NAME", Args: cobra.ExactArgs(1), Short: "Inspect a container or image",
		RunE: withStore(func(cmd *cobra.Command, e *engine.Engine, args []string) error {
			var value any
			var err error
			switch kind {
			case "container":
				value, err = e.Inspect(cmd.Context(), args[0])
			case "image":
				value, err = e.InspectImage(args[0])
			default:
				return fmt.Errorf("unknown inspect type %q", kind)
			}
			if err != nil {
				return err
			}
			if format == "" {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(value)
			}
			t, err := template.New("inspect").Option("missingkey=error").Parse(format + "\n")
			if err != nil {
				return err
			}
			return t.Execute(cmd.OutOrStdout(), value)
		}),
	}
	inspect.Flags().StringVar(&format, "format", "", "Go template")
	inspect.Flags().StringVarP(&kind, "type", "t", "container", "container or image")
	app.AddCommand(inspect)
	addLogs(app, withStore)
	addVolumes(app, withStore)
	addAttach(app, withStore)
	addTransfer(app, withStore)
	app.AddCommand(&cobra.Command{
		Use: "__monitor MODE NAME", Hidden: true, Args: cobra.ExactArgs(2),
		RunE: withStore(func(cmd *cobra.Command, e *engine.Engine, args []string) error {
			if args[0] == "start" {
				container, err := e.Inspect(cmd.Context(), args[1])
				if err != nil {
					return err
				}
				stdio, closeIO, err := monitorStreams(cmd, e, args[1], container.Options.Interactive)
				if err != nil {
					return err
				}
				defer closeIO()
				code, err := e.Start(cmd.Context(), args[1], stdio)
				return result(code, err)
			}
			if args[0] != "run" {
				return fmt.Errorf("invalid monitor mode")
			}
			var options engine.RunOptions
			if err := json.NewDecoder(cmd.InOrStdin()).Decode(&options); err != nil {
				return err
			}
			stdio, closeIO, err := monitorStreams(cmd, e, args[1], options.Interactive)
			if err != nil {
				return err
			}
			defer closeIO()
			code, err := e.Run(cmd.Context(), options, stdio)
			return result(code, err)
		}),
	})
	return app
}

func parseMount(e *engine.Engine, spec string) (engine.Mount, error) {
	parts := strings.Split(spec, ":")
	if len(parts) > 3 || len(parts) == 0 || parts[0] == "" {
		return engine.Mount{}, fmt.Errorf("invalid mount: %s", spec)
	}
	source := parts[0]
	target := source
	if len(parts) > 1 {
		target = parts[1]
	}
	if !filepath.IsAbs(target) {
		return engine.Mount{}, fmt.Errorf("mount target must be absolute: %s", target)
	}
	if !filepath.IsAbs(source) {
		if strings.ContainsAny(source, `/\`) {
			return engine.Mount{}, fmt.Errorf("mount source must be absolute or a volume name: %s", source)
		}
		if err := engine.ValidateName(source); err != nil {
			return engine.Mount{}, err
		}
		source = filepath.Join(e.Root, "volumes", source, "_data")
		if err := os.MkdirAll(source, 0700); err != nil {
			return engine.Mount{}, err
		}
	}
	if _, err := os.Stat(source); err != nil {
		return engine.Mount{}, err
	}
	mount := engine.Mount{Source: source, Target: target}
	if len(parts) == 3 {
		for _, option := range strings.Split(parts[2], ",") {
			switch option {
			case "ro":
				mount.ReadOnly = true
			case "rw", "z", "Z", "":
			default:
				return engine.Mount{}, fmt.Errorf("unsupported mount option %q", option)
			}
		}
	}
	return mount, nil
}

func launch(cmd *cobra.Command, e *engine.Engine, mode, name string, opts *engine.RunOptions) error {
	if err := engine.ValidateName(name); err != nil {
		return err
	}
	// Refuse duplicate runs before spawning a monitor that would poll an old process.
	existing, err := e.Inspect(cmd.Context(), name)
	if err == nil && (mode == "run" || existing.Status == "running") {
		return fmt.Errorf("container %s already exists or is running", name)
	}
	if mode == "start" && err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	var args []string
	// Preserve the command's embedding path (for example: ws spod).
	for node := cmd.Parent(); node != nil && node.Parent() != nil; node = node.Parent() {
		args = append([]string{node.Name()}, args...)
	}
	args = append(args, "--root", e.Root, "__monitor", mode, name)
	child := exec.Command(self, args...)
	detachProcess(child)
	payload, err := json.Marshal(opts)
	if err != nil {
		return err
	}
	child.Stdin = bytes.NewReader(payload)
	logPath := filepath.Join(e.Root, "logs", name+".log")
	if mode == "run" {
		if _, err := os.Lstat(logPath); err == nil {
			if err := e.Discard(logPath); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	child.Stdout, child.Stderr = log, log
	if err := child.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			state, err := e.Inspect(cmd.Context(), name)
			if err == nil && state.Status == "running" {
				fmt.Fprintln(cmd.OutOrStdout(), name)
				return nil
			}
		case err := <-done:
			if err == nil {
				fmt.Fprintln(cmd.OutOrStdout(), name)
				return nil
			}
			return fmt.Errorf("container %s failed: %w (see %s)", name, err, logPath)
		case <-timer.C:
			_ = child.Process.Kill()
			<-done
			_ = e.Stop(context.Background(), name, time.Second)
			return fmt.Errorf("container %s did not become ready (see %s)", name, logPath)
		case <-cmd.Context().Done():
			_ = child.Process.Kill()
			<-done
			_ = e.Stop(context.Background(), name, time.Second)
			return cmd.Context().Err()
		}
	}
}

type storeHandler func(func(*cobra.Command, *engine.Engine, []string) error) func(*cobra.Command, []string) error

func addLogs(app *cobra.Command, withStore storeHandler) {
	var follow bool
	var tail int
	logs := &cobra.Command{
		Use: "logs NAME", Args: cobra.ExactArgs(1), Short: "Read a detached container's output",
		RunE: withStore(func(cmd *cobra.Command, e *engine.Engine, args []string) error {
			if err := engine.ValidateName(args[0]); err != nil {
				return err
			}
			file, err := os.Open(filepath.Join(e.Root, "logs", args[0]+".log"))
			if err != nil {
				return err
			}
			defer file.Close()
			if tail > 0 {
				data, err := io.ReadAll(file)
				if err != nil {
					return err
				}
				lines := bytes.SplitAfter(data, []byte("\n"))
				if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
					lines = lines[:len(lines)-1]
				}
				if len(lines) > tail {
					lines = lines[len(lines)-tail:]
				}
				if _, err := cmd.OutOrStdout().Write(bytes.Join(lines, nil)); err != nil {
					return err
				}
			}
			for {
				if _, err := io.Copy(cmd.OutOrStdout(), file); err != nil {
					return err
				}
				if !follow {
					return nil
				}
				select {
				case <-cmd.Context().Done():
					return cmd.Context().Err()
				case <-time.After(200 * time.Millisecond):
				}
				container, err := e.Inspect(cmd.Context(), args[0])
				if err != nil || container.Status != "running" {
					_, err := io.Copy(cmd.OutOrStdout(), file)
					return err
				}
			}
		}),
	}
	logs.Flags().BoolVarP(&follow, "follow", "f", false, "Follow log output")
	logs.Flags().IntVar(&tail, "tail", 0, "Show last N lines (0: all)")
	app.AddCommand(logs)
}

func addVolumes(app *cobra.Command, withStore storeHandler) {
	volume := &cobra.Command{Use: "volume", Short: "Manage named volumes"}
	var format string
	list := &cobra.Command{
		Use: "ls", Args: cobra.NoArgs,
		RunE: withStore(func(cmd *cobra.Command, e *engine.Engine, _ []string) error {
			entries, err := os.ReadDir(filepath.Join(e.Root, "volumes"))
			if err != nil {
				return err
			}
			for _, entry := range entries {
				if !entry.IsDir() {
					continue
				}
				value := struct{ Name, Driver, Mountpoint string }{entry.Name(), "local", filepath.Join(e.Root, "volumes", entry.Name(), "_data")}
				if format == "" {
					if err := json.NewEncoder(cmd.OutOrStdout()).Encode(value); err != nil {
						return err
					}
					continue
				}
				t, err := template.New("volume").Parse(format + "\n")
				if err != nil {
					return err
				}
				if err := t.Execute(cmd.OutOrStdout(), value); err != nil {
					return err
				}
			}
			return nil
		}),
	}
	list.Flags().StringVar(&format, "format", "", "Go template")
	volume.AddCommand(list)
	remove := &cobra.Command{
		Use: "rm NAME", Args: cobra.ExactArgs(1),
		RunE: withStore(func(cmd *cobra.Command, e *engine.Engine, args []string) error {
			if err := engine.ValidateName(args[0]); err != nil {
				return err
			}
			path := filepath.Join(e.Root, "volumes", args[0])
			containers, err := e.List(cmd.Context())
			if err != nil {
				return err
			}
			for _, container := range containers {
				for _, mount := range container.Options.Mounts {
					rel, err := filepath.Rel(path, mount.Source)
					if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
						return fmt.Errorf("volume %s is used by container %s", args[0], container.Name)
					}
				}
			}
			return e.Discard(path)
		}),
	}
	remove.Flags().BoolP("force", "f", false, "Accepted for compatibility; volumes in use are never removed")
	volume.AddCommand(remove)
	app.AddCommand(volume)
}

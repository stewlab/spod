// SPDX-License-Identifier: Apache-2.0
//go:build linux && cgo

package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
)

// A rootless user cannot chown files to its subordinate IDs, nor reliably read
// files a container created under one. Operations that need either re-execute
// the binary inside a user namespace with the storage mapping (container root
// is the caller, container ID n>0 is subordinate ID base+n-1), where the
// helper is root and holds every capability over the mapped IDs.
//
// Two stages are needed: the clone(2)'d child cannot gain capabilities until
// newuidmap has written its mapping, and capabilities are granted at execve
// to a process that is root in its namespace. Stage one waits for the mapping
// on fd 3, then re-executes itself as stage two, which reads its request from
// fd 4 and performs the operation. Long-running operations report progress
// line by line on fd 5; stdio is the caller's.
const (
	usernsHelperEnv = "_SPOD_USERNS_HELPER"
	usernsStageEnv  = "_SPOD_USERNS_STAGE"
)

// usernsRequest is the operation a helper performs.
type usernsRequest struct {
	Op string `json:"op"`
	// Path is the directory tree the operation applies to.
	Path string `json:"path"`
	// UID and GID select the keep-id shift: the container ID that the
	// caller's host ID is mapped to.
	UID int `json:"uid,omitempty"`
	GID int `json:"gid,omitempty"`
	// Payload carries operation-specific JSON.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// usernsOps are the operations a helper can run; each reads its request and
// writes any result to stdout. Other files register theirs in init.
var usernsOps = map[string]func(usernsRequest, io.Writer) error{
	"keep-id-shift": func(req usernsRequest, _ io.Writer) error {
		return shiftTree(req.Path, keepIDShift(req.UID), keepIDShift(req.GID))
	},
	"run-init": runInitOp,
}

// runUsernsHelper is the helper-process entry point, dispatched from Init. It
// never returns.
func runUsernsHelper() {
	if os.Getenv(usernsStageEnv) != "2" {
		sync := os.NewFile(3, "userns-sync")
		var b [1]byte
		if _, err := io.ReadFull(sync, b[:]); err != nil {
			os.Exit(125)
		}
		sync.Close()
		env := append(os.Environ(), usernsStageEnv+"=2")
		err := syscall.Exec("/proc/self/exe", os.Args, env)
		fmt.Fprintln(os.Stderr, "spod userns helper: re-exec:", err)
		os.Exit(125)
	}
	usernsReport = os.NewFile(5, "userns-report")
	var req usernsRequest
	if err := json.NewDecoder(os.NewFile(4, "userns-request")).Decode(&req); err != nil {
		fmt.Fprintln(os.Stderr, "spod userns helper: request:", err)
		os.Exit(125)
	}
	op, ok := usernsOps[req.Op]
	if !ok {
		fmt.Fprintln(os.Stderr, "spod userns helper: unknown operation", req.Op)
		os.Exit(125)
	}
	if err := op(req, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "spod userns helper:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// usernsReport is the helper's progress pipe (fd 5), set in the helper only.
var usernsReport *os.File

// inUserns runs req in a helper under the storage mapping, streaming its
// output to stdout.
func inUserns(req usernsRequest, stdout io.Writer) error {
	var stderr bytes.Buffer
	cmd, err := startUserns(req, IO{Stdout: stdout, Stderr: &stderr}, nil)
	if err != nil {
		return err
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("userns helper %s: %v: %s", req.Op, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// startUserns starts a helper for req with the given stdio and optional
// progress pipe write end, returning once it has its mapping.
func startUserns(req usernsRequest, stdio IO, report *os.File) (*exec.Cmd, error) {
	uidMaps, gidMaps := idMappings()
	if len(uidMaps) < 2 || len(gidMaps) < 2 {
		return nil, errors.New("this operation needs subordinate IDs: install newuidmap/newgidmap (shadow/uidmap) and add the user to /etc/subuid and /etc/subgid")
	}
	reqData, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	syncR, syncW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer syncW.Close()
	reqR, reqW, err := os.Pipe()
	if err != nil {
		syncR.Close()
		return nil, err
	}
	defer reqW.Close()
	if report == nil {
		if report, err = os.Open(os.DevNull); err != nil {
			syncR.Close()
			reqR.Close()
			return nil, err
		}
		defer report.Close()
	}
	cmd := exec.Command("/proc/self/exe", "spod-userns-helper")
	cmd.Env = append(filterEnv(os.Environ(), usernsStageEnv, "_LIBCONTAINER_INITPIPE"), usernsHelperEnv+"=1")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdio.Stdin, stdio.Stdout, stdio.Stderr
	cmd.ExtraFiles = []*os.File{syncR, reqR, report}
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWUSER, Pdeathsig: unix.SIGKILL}
	err = cmd.Start()
	syncR.Close()
	reqR.Close()
	if err != nil {
		return nil, fmt.Errorf("start userns helper: %w", err)
	}
	pid := strconv.Itoa(cmd.Process.Pid)
	for _, m := range []struct {
		bin  string
		maps []specs.LinuxIDMapping
	}{{"newuidmap", uidMaps}, {"newgidmap", gidMaps}} {
		args := []string{pid}
		for _, r := range m.maps {
			args = append(args, strconv.FormatUint(uint64(r.ContainerID), 10), strconv.FormatUint(uint64(r.HostID), 10), strconv.FormatUint(uint64(r.Size), 10))
		}
		if out, err := exec.Command(m.bin, args...).CombinedOutput(); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return nil, fmt.Errorf("%s: %v: %s", m.bin, err, strings.TrimSpace(string(out)))
		}
	}
	// The request fits the pipe buffer, so writing it cannot block.
	if _, err := reqW.Write(reqData); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, err
	}
	if _, err := syncW.Write([]byte{1}); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, err
	}
	return cmd, nil
}

func filterEnv(env []string, keys ...string) []string {
	out := env[:0:0]
outer:
	for _, kv := range env {
		for _, k := range keys {
			if strings.HasPrefix(kv, k+"=") {
				continue outer
			}
		}
		out = append(out, kv)
	}
	return out
}

// keepIDShift maps a container ID under the storage mapping to the storage
// ID that a keep-id container (caller mapped to container ID id) sees as the
// same container ID: IDs below id move up by one, id becomes the caller
// (storage ID 0), and IDs above id are unchanged.
func keepIDShift(id int) func(int) int {
	return func(n int) int {
		switch {
		case n < id:
			return n + 1
		case n == id:
			return 0
		default:
			return n
		}
	}
}

// shiftTree re-owns every entry under root through the given ID functions
// without following symlinks, restoring the setuid/setgid bits chown clears.
func shiftTree(root string, uidFn, gidFn func(int) int) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("no ownership information for %s", path)
		}
		uid, gid := uidFn(int(st.Uid)), gidFn(int(st.Gid))
		if uid == int(st.Uid) && gid == int(st.Gid) {
			return nil
		}
		if err := os.Lchown(path, uid, gid); err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink == 0 && info.Mode()&(fs.ModeSetuid|fs.ModeSetgid) != 0 {
			return os.Chmod(path, info.Mode())
		}
		return nil
	})
}

// nestedKeepIDMappings returns the keep-id mapping for a container created
// inside the storage user namespace, expressed in that namespace's IDs:
// storage holds the storage mapping (its total size bounds the namespace).
// Container IDs below id are namespace IDs 1..id (subordinate IDs, as under
// the default mapping), id is namespace ID 0 (the caller), and IDs above id
// map to themselves.
func nestedKeepIDMappings(id int, storage []specs.LinuxIDMapping) ([]specs.LinuxIDMapping, error) {
	var total uint64
	for _, m := range storage {
		total += uint64(m.Size)
	}
	if len(storage) < 2 || id < 0 || uint64(id) >= total {
		return nil, fmt.Errorf("--userns=keep-id needs a subordinate ID range larger than ID %d in /etc/subuid and /etc/subgid, and newuidmap/newgidmap", id)
	}
	var m []specs.LinuxIDMapping
	if id > 0 {
		m = append(m, specs.LinuxIDMapping{ContainerID: 0, HostID: 1, Size: uint32(id)})
	}
	m = append(m, specs.LinuxIDMapping{ContainerID: uint32(id), HostID: 0, Size: 1})
	if rest := total - 1 - uint64(id); rest > 0 {
		m = append(m, specs.LinuxIDMapping{ContainerID: uint32(id) + 1, HostID: uint32(id) + 1, Size: uint32(rest)})
	}
	return m, nil
}

// parseUserns validates a --userns value: "" (container root is the caller)
// or "keep-id".
func parseUserns(mode string) error {
	switch mode {
	case "", "keep-id":
		return nil
	default:
		return fmt.Errorf("unsupported --userns %q: spod supports keep-id", mode)
	}
}

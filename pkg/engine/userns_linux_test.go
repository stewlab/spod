// SPDX-License-Identifier: Apache-2.0
//go:build linux && cgo

package engine

import (
	"reflect"
	"testing"

	"github.com/opencontainers/runtime-spec/specs-go"
)

func TestKeepIDShift(t *testing.T) {
	shift := keepIDShift(1000)
	for in, want := range map[int]int{0: 1, 999: 1000, 1000: 0, 1001: 1001, 5000: 5000} {
		if got := shift(in); got != want {
			t.Errorf("keepIDShift(1000)(%d) = %d, want %d", in, got, want)
		}
	}
	// A caller that is root on the host keeps ownership unchanged.
	if got := keepIDShift(0)(0); got != 0 {
		t.Errorf("keepIDShift(0)(0) = %d, want 0", got)
	}
}

func TestNestedKeepIDMappings(t *testing.T) {
	storage := []specs.LinuxIDMapping{
		{ContainerID: 0, HostID: 1000, Size: 1},
		{ContainerID: 1, HostID: 100000, Size: 65536},
	}
	got, err := nestedKeepIDMappings(1000, storage)
	if err != nil {
		t.Fatal(err)
	}
	want := []specs.LinuxIDMapping{
		{ContainerID: 0, HostID: 1, Size: 1000},
		{ContainerID: 1000, HostID: 0, Size: 1},
		{ContainerID: 1001, HostID: 1001, Size: 64536},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("nestedKeepIDMappings = %v, want %v", got, want)
	}
	// Each container ID maps to the namespace ID keepIDShift moved its files to.
	shift := keepIDShift(1000)
	for _, id := range []int{0, 1, 999, 1000, 1001, 65536} {
		ns := -1
		for _, m := range got {
			if uint32(id) >= m.ContainerID && uint32(id) < m.ContainerID+m.Size {
				ns = int(m.HostID + uint32(id) - m.ContainerID)
			}
		}
		if ns != shift(id) {
			t.Errorf("container ID %d maps to namespace ID %d, files shifted to %d", id, ns, shift(id))
		}
	}

	if _, err := nestedKeepIDMappings(1000, storage[:1]); err == nil {
		t.Error("expected an error without subordinate IDs")
	}
	if _, err := nestedKeepIDMappings(70000, storage); err == nil {
		t.Error("expected an error when the ID exceeds the range")
	}
}

func TestParseUserns(t *testing.T) {
	for _, ok := range []string{"", "keep-id"} {
		if err := parseUserns(ok); err != nil {
			t.Errorf("parseUserns(%q): %v", ok, err)
		}
	}
	if err := parseUserns("host"); err == nil {
		t.Error("parseUserns(host) should fail")
	}
}

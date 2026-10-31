package engine

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseContainerfile(t *testing.T) {
	cf, err := parseContainerfile(strings.NewReader(`# syntax=docker/dockerfile:1
ARG BASE=alpine
FROM $BASE AS build
# a comment
RUN echo one \
    # comment inside a continuation
    two
COPY --chown=1:2 --from=other a b /dst/
CMD ["sh", "-c", "echo hi"]
from scratch
ENTRYPOINT run me
`))
	if err != nil {
		t.Fatal(err)
	}
	if cf.Escape != '\\' || len(cf.GlobalArgs) != 1 || len(cf.Stages) != 2 {
		t.Fatalf("got escape %q, %d global args, %d stages", cf.Escape, len(cf.GlobalArgs), len(cf.Stages))
	}
	s := cf.Stages[0]
	if s.Name != "build" || len(s.Instructions) != 3 {
		t.Fatalf("stage 0: name %q, %d instructions", s.Name, len(s.Instructions))
	}
	run := s.Instructions[0]
	if run.Cmd != "run" || run.Line != 5 || strings.Join(strings.Fields(run.Rest), " ") != "echo one two" {
		t.Fatalf("RUN: %+v", run)
	}
	cp := s.Instructions[1]
	if cp.Flags["chown"] != "1:2" || cp.Flags["from"] != "other" || cp.Rest != "a b /dst/" {
		t.Fatalf("COPY: %+v", cp)
	}
	cmd := s.Instructions[2]
	if !cmd.IsJSON || !reflect.DeepEqual(cmd.JSON, []string{"sh", "-c", "echo hi"}) {
		t.Fatalf("CMD: %+v", cmd)
	}
	if ep := cf.Stages[1].Instructions[0]; ep.IsJSON || ep.Rest != "run me" {
		t.Fatalf("ENTRYPOINT: %+v", ep)
	}
}

func TestParseContainerfileEscapeDirective(t *testing.T) {
	cf, err := parseContainerfile(strings.NewReader("# escape=`\nFROM scratch\nRUN dir C:\\ `\n  /w\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cf.Escape != '`' {
		t.Fatalf("escape %q", cf.Escape)
	}
	if got := strings.Join(strings.Fields(cf.Stages[0].Instructions[0].Rest), " "); got != `dir C:\ /w` {
		t.Fatalf("RUN %q", got)
	}
}

func TestParseContainerfileErrors(t *testing.T) {
	for name, text := range map[string]string{
		"no from":        "RUN true\n",
		"heredoc":        "FROM scratch\nRUN <<EOF\ntrue\nEOF\n",
		"run mount":      "FROM scratch\nRUN --mount=type=cache,target=/c true\n",
		"unknown":        "FROM scratch\nFROBNICATE x\n",
		"bad from":       "FROM a b\n",
		"bad escape":     "# escape=x\nFROM scratch\n",
		"unknown flag":   "FROM scratch\nCOPY --nope a b\n",
		"bad shell form": "FROM scratch\nSHELL sh -c\n",
	} {
		if _, err := parseContainerfile(strings.NewReader(text)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestExpandWords(t *testing.T) {
	vars := map[string]string{"A": "1", "E": "", "S": "x y"}
	lookup := func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
	for in, want := range map[string][]string{
		`$A ${A}b`:                     {"1", "1b"},
		`${U:-d} ${E:-d} ${E-d}`:       {"d", "d"},
		`${A:+set} ${E:+set} ${U+set}`: {"set"},
		`"$S" '$S' \$A`:                {"x y", "$S", "$A"},
		`$S`:                           {"x y"},
		`a\ b`:                         {"a b"},
	} {
		got, err := expandWords(in, lookup, '\\', true)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("expandWords(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := expandWords(`${A/x/y}`, lookup, '\\', true); err == nil {
		t.Error("pattern substitution accepted")
	}
}

func TestKeyValues(t *testing.T) {
	lookup := func(k string) (string, bool) { return "v", k == "X" }
	for _, c := range []struct {
		line string
		want [][2]string
	}{
		{"ENV A=1 B=\"two words\" C=$X", [][2]string{{"A", "1"}, {"B", "two words"}, {"C", "v"}}},
		{"ENV PATH /usr/bin:$X", [][2]string{{"PATH", "/usr/bin:v"}}},
		{"LABEL a.b=c", [][2]string{{"a.b", "c"}}},
		{"ARG N", [][2]string{{"N", "\x00"}}},
		{"ARG N=1 M", [][2]string{{"N", "1"}, {"M", "\x00"}}},
	} {
		ins, err := parseInstruction(c.line, 1)
		if err != nil {
			t.Fatal(err)
		}
		got, err := keyValues(ins, lookup, '\\')
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %q, %v; want %q", c.line, got, err, c.want)
		}
	}
}

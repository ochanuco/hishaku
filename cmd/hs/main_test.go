package main

import (
	"flag"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestParseInterspersed(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	takeover := fs.Bool("takeover", false, "")
	pos, err := parseInterspersed(fs, []string{"4821", "--takeover"})
	if err != nil || !*takeover || !reflect.DeepEqual(pos, []string{"4821"}) {
		t.Fatalf("pos = %v takeover = %v err = %v", pos, *takeover, err)
	}
}

func TestParseNumber(t *testing.T) {
	for _, s := range []string{"999", "10000", "abc", ""} {
		if _, err := parseNumber(s); err == nil {
			t.Errorf("%q should fail", s)
		}
	}
	if n, err := parseNumber("4821"); err != nil || n != 4821 {
		t.Fatalf("%d %v", n, err)
	}
}

func TestRunUsage(t *testing.T) {
	var out, errOut strings.Builder
	if code := run([]string{"--help"}, strings.NewReader(""), &out, &errOut); code != 0 || !strings.Contains(out.String(), "hs resume") {
		t.Fatalf("code = %d out = %q", code, out.String())
	}
	if code := run([]string{"nope"}, strings.NewReader(""), &out, &errOut); code != 2 {
		t.Fatalf("unknown command code = %d", code)
	}
	if code := run([]string{"open", "bad", "4821"}, strings.NewReader(""), &out, &errOut); code != 2 {
		t.Fatalf("bad repo code = %d", code)
	}
}

func TestSplitExtra(t *testing.T) {
	own, extra := splitExtra([]string{"4821", "--takeover", "--", "--model", "x", "--"})
	if !reflect.DeepEqual(own, []string{"4821", "--takeover"}) || !reflect.DeepEqual(extra, []string{"--model", "x", "--"}) {
		t.Fatalf("own = %v extra = %v", own, extra)
	}
	if own, extra := splitExtra([]string{"4821"}); len(own) != 1 || extra != nil {
		t.Fatalf("own = %v extra = %v", own, extra)
	}
}

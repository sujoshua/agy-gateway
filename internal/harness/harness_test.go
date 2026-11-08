package harness

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture creates a runtime binary with a harness next to it and a patch
// script that appends to its argument and counts its runs.
func fixture(t *testing.T, script string) (o Options, runs func() int) {
	t.Helper()
	dir := t.TempDir()
	rt := filepath.Join(dir, "runtime", "agy_acp_server.par")
	if err := os.MkdirAll(filepath.Dir(rt), 0o750); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{rt, filepath.Join(filepath.Dir(rt), FileName)} {
		if err := os.WriteFile(f, []byte("original"), 0o500); err != nil {
			t.Fatal(err)
		}
	}
	count := filepath.Join(dir, "count")
	patch := filepath.Join(dir, "patch.sh")
	body := "#!/bin/sh\necho run >> " + count + "\n" + script + "\n"
	if err := os.WriteFile(patch, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	o = Options{RuntimeCmd: rt, PatchCmd: []string{"/bin/sh", patch}, CacheDir: filepath.Join(dir, "cache")}
	return o, func() int {
		b, _ := os.ReadFile(count)
		return strings.Count(string(b), "run")
	}
}

func TestPrepareCopiesPatchesAndCaches(t *testing.T) {
	o, runs := fixture(t, `for last; do :; done; printf -- '-patched' >> "$last"`)
	path, err := Prepare(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "original-patched" {
		t.Fatalf("patched copy = %q", got)
	}
	orig, _ := os.ReadFile(filepath.Join(filepath.Dir(o.RuntimeCmd), FileName))
	if string(orig) != "original" {
		t.Fatalf("original was modified: %q", orig)
	}
	again, err := Prepare(context.Background(), o)
	if err != nil || again != path || runs() != 1 {
		t.Fatalf("second Prepare: %q %v, runs %d", again, err, runs())
	}
	// A different command means a different copy.
	o.PatchCmd = append(o.PatchCmd, "--flag")
	other, err := Prepare(context.Background(), o)
	if err != nil || other == path || runs() != 2 {
		t.Fatalf("changed command: %q %v, runs %d", other, err, runs())
	}
}

func TestPrepareFailures(t *testing.T) {
	for name, script := range map[string]string{
		"exit status": "echo broken >&2; exit 3",
		"no change":   "true",
	} {
		t.Run(name, func(t *testing.T) {
			o, _ := fixture(t, script)
			if _, err := Prepare(context.Background(), o); err == nil {
				t.Fatal("want an error")
			}
			entries, _ := filepath.Glob(filepath.Join(o.CacheDir, "*", "*"))
			if len(entries) != 0 {
				t.Fatalf("left files behind: %v", entries)
			}
		})
	}
}

func TestPrepareExplicitSource(t *testing.T) {
	o, _ := fixture(t, `printf x >> "$1"`)
	src := filepath.Join(t.TempDir(), "harness")
	if err := os.WriteFile(src, []byte("custom"), 0o600); err != nil {
		t.Fatal(err)
	}
	o.Source, o.RuntimeCmd = src, "does-not-exist"
	path, err := Prepare(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "customx" {
		t.Fatalf("patched copy = %q", got)
	}
}

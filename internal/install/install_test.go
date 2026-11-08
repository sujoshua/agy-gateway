package install

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func archive(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{Command: "#!/bin/sh\n", "localharness_external": "harness", "../escape": "x"} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestEnsure(t *testing.T) {
	if _, ok := archNames[runtime.GOARCH]; runtime.GOOS != "linux" || !ok {
		t.Skip("the runtime download is linux only")
	}
	zipBytes := archive(t)
	sum := sha256.Sum256(zipBytes)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if !strings.HasSuffix(r.URL.Path, "/agy-acp-server-9.9.9-linux-"+archNames[runtime.GOARCH]+".zip") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(zipBytes)
	}))
	defer srv.Close()
	dir := t.TempDir()
	o := Options{Version: "9.9.9", SHA256: hex.EncodeToString(sum[:]), Dir: dir, BaseURL: srv.URL}

	cmd, err := Ensure(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	if cmd != filepath.Join(dir, "9.9.9", Command) {
		t.Fatalf("cmd %s", cmd)
	}
	if st, err := os.Stat(cmd); err != nil || st.Mode().Perm()&0o100 == 0 {
		t.Fatalf("runtime not executable: %v %v", st, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "9.9.9", "localharness_external")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "escape")); !os.IsNotExist(err) {
		t.Fatal("an unexpected archive entry was extracted")
	}
	if _, err := Ensure(t.Context(), o); err != nil || hits.Load() != 1 {
		t.Fatalf("second Ensure: %v, %d downloads", err, hits.Load())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("leftover files: %v", entries)
	}
}

func TestEnsureChecksumMismatch(t *testing.T) {
	if _, ok := archNames[runtime.GOARCH]; runtime.GOOS != "linux" || !ok {
		t.Skip("the runtime download is linux only")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive(t))
	}))
	defer srv.Close()
	dir := t.TempDir()
	_, err := Ensure(t.Context(), Options{Version: "9.9.9", SHA256: strings.Repeat("0", 64), Dir: dir, BaseURL: srv.URL})
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("err %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("leftover files: %v", entries)
	}
}

func TestEnsureNeedsChecksum(t *testing.T) {
	if _, ok := archNames[runtime.GOARCH]; runtime.GOOS != "linux" || !ok {
		t.Skip("the runtime download is linux only")
	}
	if _, err := Ensure(t.Context(), Options{Version: "0.0.1", Dir: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "AGY_GW_RUNTIME_SHA256") {
		t.Fatalf("err %v", err)
	}
}

func TestEnsurePrunes(t *testing.T) {
	if _, ok := archNames[runtime.GOARCH]; runtime.GOOS != "linux" || !ok {
		t.Skip("the runtime download is linux only")
	}
	dir := t.TempDir()
	mk := func(name string, age time.Duration) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-age)
		if err := os.Chtimes(filepath.Join(dir, name), at, at); err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range []string{"1.0.0", "2.0.0"} {
		if err := os.MkdirAll(filepath.Join(dir, v), 0o700); err != nil {
			t.Fatal(err)
		}
		mk(filepath.Join(v, Command), 0)
	}
	if err := os.MkdirAll(filepath.Join(dir, "other"), 0o700); err != nil {
		t.Fatal(err)
	}
	mk("download-old.zip", 2*staleAfter)
	mk("download-new.zip", 0)
	mk("unrelated", 2*staleAfter)

	left := func() []string {
		entries, _ := os.ReadDir(dir)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		return names
	}
	o := Options{Version: "2.0.0", SHA256: strings.Repeat("0", 64), Dir: dir, KeepOld: true}
	if _, err := Ensure(t.Context(), o); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(left(), " "); got != "1.0.0 2.0.0 download-new.zip other unrelated" {
		t.Fatalf("KeepOld left %q", got)
	}
	o.KeepOld = false
	if _, err := Ensure(t.Context(), o); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(left(), " "); got != "2.0.0 download-new.zip other unrelated" {
		t.Fatalf("left %q", got)
	}
}

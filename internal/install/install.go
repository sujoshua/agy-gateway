// Package install downloads the official Antigravity ACP runtime from
// Google's release bucket when it is not already present. The archive is
// verified against a pinned SHA-256 before anything is unpacked.
package install

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// DefaultVersion is the runtime version downloaded when none is configured.
const DefaultVersion = "1.4.5"

// BaseURL is where Google publishes the Linux runtime archives.
const BaseURL = "https://dl.google.com/agy-extensions/releases/linux"

// Command is the runtime executable inside the archive. The harness next to
// it is found by the runtime itself.
const Command = "agy_acp_server.par"

var files = []string{Command, "localharness_external"}

// checksums are the SHA-256 sums of the official archives, by version and
// GOARCH.
var checksums = map[string]map[string]string{
	"1.4.5": {
		"amd64": "923923a0d7dc8073f66cc6f78647a3107e3a7ee2b7e1a24df8b3927b5188013b",
		"arm64": "7d64246633ae094f67e3bbcd2d679e89048f91346d5339313336e7483f40f056",
	},
}

// archNames maps GOARCH to the architecture in the archive name.
var archNames = map[string]string{"amd64": "x86_64", "arm64": "arm64"}

// staleAfter is how old an unfinished download or unpack must be before
// Prune removes it, so a download still in progress elsewhere survives.
const staleAfter = time.Hour

// maxFile bounds one unpacked file; the 1.4.5 runtime is about 830 MB.
const maxFile = 4 << 30

// Options configures Ensure.
type Options struct {
	Version string
	// SHA256 is the archive checksum; empty uses the pinned one.
	SHA256 string
	// Dir holds unpacked runtimes, one directory per version.
	Dir string
	// BaseURL overrides the download location (tests).
	BaseURL string
	// KeepOld keeps other versions in Dir; by default they are removed once
	// Version is in place.
	KeepOld bool
	Logger  *slog.Logger
}

// Ensure returns the path of the runtime executable for o.Version,
// downloading and unpacking the official archive into o.Dir first if that
// version is not there yet.
func Ensure(ctx context.Context, o Options) (string, error) {
	if o.Version == "" {
		o.Version = DefaultVersion
	}
	if o.BaseURL == "" {
		o.BaseURL = BaseURL
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	arch, ok := archNames[runtime.GOARCH]
	if runtime.GOOS != "linux" || !ok {
		return "", fmt.Errorf("the runtime download supports linux/amd64 and linux/arm64, not %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	sum := o.SHA256
	if sum == "" {
		sum = checksums[o.Version][runtime.GOARCH]
	}
	if sum == "" {
		return "", fmt.Errorf("no pinned checksum for runtime %s on linux/%s; set AGY_GW_RUNTIME_SHA256", o.Version, runtime.GOARCH)
	}
	dest := filepath.Join(o.Dir, o.Version)
	cmd := filepath.Join(dest, Command)
	if _, err := os.Stat(cmd); err == nil {
		prune(o)
		return cmd, nil
	}
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return "", err
	}
	url := fmt.Sprintf("%s/agy-acp-server-%s-linux-%s.zip", o.BaseURL, o.Version, arch)
	o.Logger.Info("downloading the runtime", slog.String("url", url))
	start := time.Now()
	archive, err := download(ctx, url, o.Dir, sum)
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(archive) }()
	o.Logger.Info("runtime downloaded and verified", slog.String("elapsed", time.Since(start).Round(time.Second).String()))
	tmp, err := os.MkdirTemp(o.Dir, "unpack-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if err := unpack(archive, tmp); err != nil {
		return "", err
	}
	// The rename publishes a complete directory, so an interrupted
	// download never leaves a half-unpacked runtime behind.
	if err := os.Rename(tmp, dest); err != nil {
		return "", err
	}
	o.Logger.Info("runtime installed", slog.String("path", cmd))
	prune(o)
	return cmd, nil
}

// prune removes what Ensure left behind in o.Dir: unfinished downloads and
// unpacks older than staleAfter and, unless o.KeepOld, every other version.
// Only entries Ensure creates are touched; anything else in o.Dir stays.
// Failures are logged, since the runtime is usable either way.
func prune(o Options) {
	entries, err := os.ReadDir(o.Dir)
	if err != nil {
		o.Logger.Warn("listing downloaded runtimes failed", slog.Any("err", err))
		return
	}
	for _, e := range entries {
		name := e.Name()
		path := filepath.Join(o.Dir, name)
		switch {
		case name == o.Version:
			continue
		case strings.HasPrefix(name, "download-") || strings.HasPrefix(name, "unpack-"):
			info, err := e.Info()
			if err != nil || time.Since(info.ModTime()) < staleAfter {
				continue
			}
		case e.IsDir() && !o.KeepOld:
			if _, err := os.Stat(filepath.Join(path, Command)); err != nil {
				continue
			}
		default:
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			o.Logger.Warn("removing an old runtime failed", slog.String("path", path), slog.Any("err", err))
			continue
		}
		o.Logger.Info("removed an old runtime", slog.String("path", path))
	}
}

// download saves url into dir and checks its SHA-256 against want.
func download(ctx context.Context, url, dir, want string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	f, err := os.CreateTemp(dir, "download-*.zip")
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && hex.EncodeToString(h.Sum(nil)) != want {
		err = fmt.Errorf("checksum mismatch for %s", url)
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// unpack extracts the runtime files from the archive into dir. Other
// entries are ignored, so archive paths never choose where files go.
func unpack(archive, dir string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer func() { _ = zr.Close() }()
	for _, name := range files {
		f, err := zr.Open(name)
		if err != nil {
			return fmt.Errorf("archive has no %s: %w", name, err)
		}
		err = extract(f, filepath.Join(dir, name))
		_ = f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func extract(r io.Reader, path string) error {
	out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755) //nolint:gosec // G302: the runtime must be executable.
	if err != nil {
		return err
	}
	n, err := io.CopyN(out, r, maxFile+1)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	if err == nil && n > maxFile {
		err = fmt.Errorf("%s is larger than %d bytes", filepath.Base(path), int64(maxFile))
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

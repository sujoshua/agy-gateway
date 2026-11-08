// Package harness prepares the runtime's localharness_external binary. By
// default the runtime uses the one next to itself and this package does
// nothing. When the operator configures a patch command (for example a
// context-window unlock), the harness is copied into a cache directory, the
// command patches the copy, and the runtime is pointed at it through
// ANTIGRAVITY_HARNESS_PATH. The original file is never written.
package harness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// FileName is the harness binary the runtime looks for next to itself.
const FileName = "localharness_external"

// EnvVar tells the runtime where its harness is.
const EnvVar = "ANTIGRAVITY_HARNESS_PATH"

// Options configures Prepare.
type Options struct {
	// Source is the official harness. Empty means FileName next to
	// RuntimeCmd (after resolving symlinks).
	Source     string
	RuntimeCmd string
	// PatchCmd is the command and arguments that patch a binary in place;
	// the path of the copy is appended as the last argument.
	PatchCmd []string
	CacheDir string
	Timeout  time.Duration
	Logger   *slog.Logger
}

// Prepare returns the harness the runtime should use: the patched copy,
// reused from the cache when the original and the command are unchanged.
func Prepare(ctx context.Context, o Options) (string, error) {
	if len(o.PatchCmd) == 0 {
		return "", errors.New("no patch command")
	}
	src, err := source(o)
	if err != nil {
		return "", err
	}
	srcSum, err := fileSum(src)
	if err != nil {
		return "", fmt.Errorf("harness %s: %w", src, err)
	}
	key := sha256.Sum256([]byte(srcSum + "\x00" + strings.Join(o.PatchCmd, "\x00")))
	dir := filepath.Join(o.CacheDir, hex.EncodeToString(key[:8]))
	dst := filepath.Join(dir, FileName)
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := copyExecutable(src, dir)
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(tmp) }()

	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := slices.Concat(o.PatchCmd[1:], []string{tmp})
	cmd := exec.CommandContext(ctx, o.PatchCmd[0], args...) //nolint:gosec // G204: the patch command is operator configuration, not request input.
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	runErr := cmd.Run()
	if o.Logger != nil {
		o.Logger.Info("harness patch command finished", slog.String("output", tail(out.String(), 4096)))
	}
	if runErr != nil {
		return "", fmt.Errorf("patch command: %w: %s", runErr, tail(out.String(), 512))
	}
	sum, err := fileSum(tmp)
	if err != nil {
		return "", err
	}
	if sum == srcSum {
		return "", errors.New("patch command left the harness unchanged; is it the right tool for this runtime version?")
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", err
	}
	return dst, nil
}

func source(o Options) (string, error) {
	if o.Source != "" {
		return o.Source, nil
	}
	bin, err := exec.LookPath(o.RuntimeCmd)
	if err != nil {
		return "", fmt.Errorf("runtime %q: %w", o.RuntimeCmd, err)
	}
	if bin, err = filepath.EvalSymlinks(bin); err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(bin), FileName), nil
}

func fileSum(path string) (string, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyExecutable(src, dir string) (path string, err error) {
	in, err := os.Open(filepath.Clean(src))
	if err != nil {
		return "", err
	}
	defer func() { _ = in.Close() }()
	out, err := os.CreateTemp(dir, FileName+".*.tmp")
	if err != nil {
		return "", err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(out.Name())
		}
	}()
	if _, err = io.Copy(out, in); err == nil {
		err = out.Chmod(0o700)
	}
	if err != nil {
		return "", err
	}
	return out.Name(), nil
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}

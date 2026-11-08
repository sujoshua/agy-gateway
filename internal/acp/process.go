package acp

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ExecOptions describes how to launch the runtime binary.
type ExecOptions struct {
	Cmd  string
	Args []string
	// Home overrides HOME so the runtime keeps its state on a volume.
	Home string
	Dir  string
	// Env is added to the gateway's own environment.
	Env []string
	// Logger receives stderr lines at debug level, redacted.
	Logger *slog.Logger
}

// ExecStarter launches the runtime as a child process in its own process
// group so a restart never leaves helpers behind.
func ExecStarter(o ExecOptions) Starter {
	return func(ctx context.Context) (*Process, error) {
		cmd := exec.CommandContext(ctx, o.Cmd, o.Args...) //nolint:gosec // G204: the runtime command is operator configuration, not request input.
		cmd.Dir = o.Dir
		cmd.Env = append(os.Environ(), o.Env...)
		if o.Home != "" {
			cmd.Env = append(cmd.Env, "HOME="+o.Home)
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			return nil, err
		}
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		authURLs := make(chan string, 1)
		go scanStderr(stderr, o.Logger, authURLs)
		done := make(chan struct{})
		go func() {
			_ = cmd.Wait()
			close(done)
		}()
		var once sync.Once
		kill := func() {
			once.Do(func() {
				_ = stdin.Close()
				// Give the runtime a moment to exit on EOF, then kill the group.
				select {
				case <-done:
					return
				case <-time.After(3 * time.Second):
				}
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				<-done
			})
		}
		return &Process{Stdin: stdin, Stdout: stdout, Done: done, Kill: kill, AuthURLs: authURLs}, nil
	}
}

var (
	sensitive = regexp.MustCompile(`(?i)(token|code=|bearer|secret|password|refresh|authorization|https?://\S*\?\S+)`)
	// authURL matches the line the runtime prints during authenticate:
	// "Open the following link to authenticate the ACP server: <url>".
	authURL = regexp.MustCompile(`https://accounts\.google\.com/\S+`)
)

// scanStderr logs stderr lines, redacted, and passes sign-in URLs to
// authURLs, keeping only the newest.
func scanStderr(r io.Reader, log *slog.Logger, authURLs chan string) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if u := authURL.FindString(line); u != "" {
			select {
			case <-authURLs:
			default:
			}
			authURLs <- u
		}
		if sensitive.MatchString(line) {
			line = "[redacted line]"
		}
		if len(line) > 400 {
			line = line[:400] + "..."
		}
		log.Debug("runtime stderr", slog.String("line", strings.TrimSpace(line)))
	}
}

// RedactingHandler drops the "raw" attribute the ACP SDK attaches to parse
// errors, so unexpected runtime output is never copied into logs.
type RedactingHandler struct{ slog.Handler }

// Handle implements slog.Handler.
func (h RedactingHandler) Handle(ctx context.Context, rec slog.Record) error {
	out := slog.NewRecord(rec.Time, rec.Level, rec.Message, rec.PC)
	rec.Attrs(func(a slog.Attr) bool {
		if a.Key == "raw" {
			a = slog.Int("raw_bytes", len(a.Value.String()))
		}
		out.AddAttrs(a)
		return true
	})
	return h.Handler.Handle(ctx, out)
}

// WithAttrs implements slog.Handler.
func (h RedactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return RedactingHandler{h.Handler.WithAttrs(attrs)}
}

// WithGroup implements slog.Handler.
func (h RedactingHandler) WithGroup(name string) slog.Handler {
	return RedactingHandler{h.Handler.WithGroup(name)}
}

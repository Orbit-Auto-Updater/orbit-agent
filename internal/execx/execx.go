// Package execx runs external commands with a timeout, a fixed C locale and a
// bounded output buffer. Runner is an interface so callers can be tested with
// canned output.
package execx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const maxOutput = 256 << 10 // per stream

type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// Combined returns stdout followed by stderr, trimmed.
func (r Result) Combined() string {
	return strings.TrimSpace(string(r.Stdout) + "\n" + string(r.Stderr))
}

type Cmd struct {
	Name    string
	Args    []string
	Env     []string // appended to the base environment
	Timeout time.Duration
}

func (c Cmd) String() string { return strings.Join(append([]string{c.Name}, c.Args...), " ") }

type Runner interface {
	// Run executes the command. A non-zero exit status is reported through
	// Result.ExitCode, not as an error; err is only set when the process could
	// not be started, timed out or was cancelled.
	Run(ctx context.Context, c Cmd) (Result, error)
	// LookPath reports whether a binary is available.
	LookPath(name string) (string, error)
}

type OS struct{}

func (OS) LookPath(name string) (string, error) { return exec.LookPath(name) }

func (OS) Run(ctx context.Context, c Cmd) (Result, error) {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Env = append(baseEnv(), c.Env...)
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr capped
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	err := cmd.Run()
	res := Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	var exitErr *exec.ExitError
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return res, fmt.Errorf("%s: timed out after %s", c.Name, timeout)
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
		return res, nil
	case err != nil:
		return res, fmt.Errorf("%s: %w", c.Name, err)
	}
	return res, nil
}

// baseEnv keeps PATH/HOME but forces a predictable locale so output parsing
// is stable across hosts.
func baseEnv() []string {
	env := []string{"LANG=C", "LC_ALL=C"}
	for _, k := range []string{"PATH", "HOME", "USER"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	if _, ok := os.LookupEnv("PATH"); !ok {
		env = append(env, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	return env
}

// capped keeps the last maxOutput bytes written to it.
type capped struct{ buf bytes.Buffer }

func (c *capped) Write(p []byte) (int, error) {
	n := len(p)
	c.buf.Write(p)
	if over := c.buf.Len() - maxOutput; over > 0 {
		c.buf.Next(over)
	}
	return n, nil
}

func (c *capped) Bytes() []byte { return c.buf.Bytes() }

// Tail returns at most n bytes from the end of s, cut at a line boundary.
func Tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	if i := strings.IndexByte(s, '\n'); i >= 0 && i < len(s)-1 {
		s = s[i+1:]
	}
	return "…\n" + s
}

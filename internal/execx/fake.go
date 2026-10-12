package execx

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// Fake is a Runner for tests. Responses are keyed by the full command line.
type Fake struct {
	mu        sync.Mutex
	Responses map[string]Result
	Binaries  map[string]bool
	Calls     []string
}

func (f *Fake) Run(ctx context.Context, c Cmd) (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	line := c.String()
	f.Calls = append(f.Calls, line)
	r, ok := f.Responses[line]
	if !ok {
		return Result{}, fmt.Errorf("fake: unexpected command %q", line)
	}
	if sink := lineSink(ctx); sink != nil {
		for _, l := range strings.Split(string(r.Stdout), "\n") {
			if l != "" {
				sink("stdout", l)
			}
		}
	}
	return r, nil
}

func (f *Fake) LookPath(name string) (string, error) {
	if f.Binaries[name] {
		return "/usr/bin/" + name, nil
	}
	return "", fmt.Errorf("%s: not found", name)
}

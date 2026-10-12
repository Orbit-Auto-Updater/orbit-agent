package execx

import (
	"context"
	"strings"
	"sync"
	"testing"
)

func TestLineSinkStreamsOutput(t *testing.T) {
	var mu sync.Mutex
	var got []string
	ctx := WithLineSink(context.Background(), func(stream, line string) {
		mu.Lock()
		got = append(got, stream+":"+line)
		mu.Unlock()
	})
	res, err := OS{}.Run(ctx, Cmd{Name: "sh", Args: []string{"-c", `printf 'one\ntwo\r50%%\rdone'; echo oops >&2`}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"stdout:one": true, "stdout:two": true, "stdout:50%": true, "stdout:done": true, "stderr:oops": true}
	for _, g := range got {
		delete(want, g)
	}
	if len(want) != 0 {
		t.Fatalf("missing lines %v (got %v)", want, got)
	}
	if !strings.Contains(string(res.Stdout), "done") {
		t.Fatal("output must still be captured")
	}
}

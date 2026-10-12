package journal

import (
	"context"
	"testing"

	"linux-dash-agent/internal/execx"
)

const sample = `{"__CURSOR":"s=abc;i=1","__REALTIME_TIMESTAMP":"1728640933000000","PRIORITY":"3","_SYSTEMD_UNIT":"docker.service","MESSAGE":"container api-gateway exited with code 137"}
{"__CURSOR":"s=abc;i=2","__REALTIME_TIMESTAMP":"1728640934000000","PRIORITY":"2","SYSLOG_IDENTIFIER":"kernel","MESSAGE":[69,88,84,52,255]}
{"__CURSOR":"s=abc;i=3","__REALTIME_TIMESTAMP":"1728640935000000","PRIORITY":"3","MESSAGE":""}
`

func TestParse(t *testing.T) {
	entries, cursor := Parse([]byte(sample))
	if cursor != "s=abc;i=3" {
		t.Fatalf("cursor = %q", cursor)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(entries))
	}
	if entries[0].Unit != "docker.service" || entries[0].Priority != 3 || entries[0].Time.Unix() != 1728640933 {
		t.Fatalf("%+v", entries[0])
	}
	if entries[1].Unit != "kernel" || entries[1].Message != "EXT4�" {
		t.Fatalf("binary message: %+v", entries[1])
	}
}

func TestReadUsesCursor(t *testing.T) {
	f := &execx.Fake{Responses: map[string]execx.Result{
		"journalctl --no-pager -o json -p err -n 200 --since=-10min":           {Stdout: []byte(sample)},
		"journalctl --no-pager -o json -p err -n 200 --after-cursor=s=abc;i=3": {Stdout: nil},
	}}
	r := &Reader{Runner: f}
	_, cur, err := r.Read(context.Background(), "")
	if err != nil || cur != "s=abc;i=3" {
		t.Fatalf("cur=%q err=%v", cur, err)
	}
	entries, cur2, err := r.Read(context.Background(), cur)
	if err != nil || len(entries) != 0 || cur2 != cur {
		t.Fatalf("second read: %v %q %v", entries, cur2, err)
	}
}

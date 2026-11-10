package main

import (
	"testing"
	"time"
)

func TestParseHostsMixedFormats(t *testing.T) {
	text := `
  "host-a:7709",
  ('深圳双线主站1', 'host-b', 7709),
  host-c
  host-b:7709
`
	got := parseHosts(text)
	want := []string{"host-a:7709", "host-b:7709", "host-c:7709"}
	if len(got) != len(want) {
		t.Fatalf("hosts=%v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("hosts[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

func TestNormalizeHost(t *testing.T) {
	cases := map[string]string{
		"host-test:7719": "host-test:7719",
		"host-test":      "host-test:7709",
		`"host-test"`:    "host-test:7709",
		"":               "",
	}
	for in, want := range cases {
		if got := normalizeHost(in); got != want {
			t.Fatalf("normalizeHost(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestPercentile(t *testing.T) {
	values := []time.Duration{10 * time.Millisecond, 30 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond}
	if got := percentile(values, 0.5); got != 20*time.Millisecond {
		t.Fatalf("median=%v, want 20ms", got)
	}
	if got := percentile(values, 1); got != 40*time.Millisecond {
		t.Fatalf("p100=%v, want 40ms", got)
	}
}

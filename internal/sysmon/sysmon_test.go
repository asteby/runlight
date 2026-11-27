package sysmon

import (
	"strings"
	"testing"
)

func TestParsePSI(t *testing.T) {
	in := "some avg10=12.34 avg60=5.00 avg300=1.00 total=999\nfull avg10=3.00 avg60=1 avg300=0 total=1\n"
	got, ok := parsePSI(strings.NewReader(in))
	if !ok || got != 12.34 {
		t.Fatalf("got %v %v", got, ok)
	}
	if _, ok := parsePSI(strings.NewReader("garbage")); ok {
		t.Fatal("garbage parsed")
	}
}

func TestParseMemAvail(t *testing.T) {
	in := "MemTotal:  4000000 kB\nMemFree: 1 kB\nMemAvailable:    2097152 kB\n"
	got, ok := parseMemAvailMB(strings.NewReader(in))
	if !ok || got != 2048 {
		t.Fatalf("got %v %v", got, ok)
	}
	if _, ok := parseMemAvailMB(strings.NewReader("MemTotal: 1 kB\n")); ok {
		t.Fatal("missing field parsed")
	}
}

func TestParseMeminfoKB(t *testing.T) {
	in := "MemTotal:       24000000 kB\nMemAvailable:   13000000 kB\nSwapTotal:       6291456 kB\nSwapFree:              0 kB\nBogus line\n"
	m := parseMeminfoKB(strings.NewReader(in))
	if m["MemTotal"] != 24000000 || m["MemAvailable"] != 13000000 || m["SwapTotal"] != 6291456 || m["SwapFree"] != 0 {
		t.Fatalf("got %v", m)
	}
	if _, ok := m["Bogus line"]; ok {
		t.Fatal("garbage parsed")
	}
}

func TestParseCgroupFiles(t *testing.T) {
	if n, ok := parseUint(strings.NewReader("1460772864\n")); !ok || n != 1460772864 {
		t.Fatalf("memory.current: %v %v", n, ok)
	}
	if _, ok := parseUint(strings.NewReader("max\n")); ok {
		t.Fatal("'max' must not parse as a number")
	}
	stat := "usage_usec 81210230\nuser_usec 70000000\nsystem_usec 11210230\nnr_periods 0\n"
	if n, ok := parseCPUUsec(strings.NewReader(stat)); !ok || n != 81210230 {
		t.Fatalf("usage_usec: %v %v", n, ok)
	}
	if _, ok := parseCPUUsec(strings.NewReader("nr_periods 0\n")); ok {
		t.Fatal("missing usage_usec parsed")
	}
}

func TestParseLoad(t *testing.T) {
	got, ok := parseLoad1(strings.NewReader("0.52 0.40 0.30 1/200 1234\n"))
	if !ok || got != 0.52 {
		t.Fatalf("got %v %v", got, ok)
	}
	if _, ok := parseLoad1(strings.NewReader("")); ok {
		t.Fatal("empty parsed")
	}
}

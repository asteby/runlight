package telemetry

import (
	"encoding/json"
	"strings"
	"testing"
)

var tmp62 = []TmpfsInfo{{Path: "/tmp", UsedMB: 6350, TotalMB: 12288}, {Path: "/dev/shm", UsedMB: 12, TotalMB: 11731}}

func TestVerdictNone(t *testing.T) {
	m := BuildMemory(0, 6144, 5000, 5000, 50, tmp62, nil)
	if m.Verdict != "none" || m.Note == "" {
		t.Fatalf("%+v", m)
	}
}

func TestVerdictIdleExplainsTmp(t *testing.T) {
	m := BuildMemory(6144, 6144, 0, 0, 0, tmp62, nil)
	if m.Verdict != "idle" {
		t.Fatalf("%+v", m)
	}
	want := "Swap ocupado pero inactivo: son páginas viejas que nadie usa; no hay impacto. /tmp (en RAM) guarda 6.2 GB de archivos temporales."
	if m.Note != want {
		t.Fatalf("note = %q", m.Note)
	}
}

func TestVerdictIdleSmallTmpNoMention(t *testing.T) {
	small := []TmpfsInfo{{Path: "/tmp", UsedMB: 1024, TotalMB: 12288}} // exactly 1 GB: not "> 1 GB"
	m := BuildMemory(6144, 6144, 10, 20, 0.5, small, nil)
	if m.Verdict != "idle" || strings.Contains(m.Note, "/tmp") {
		t.Fatalf("%+v", m)
	}
	// /dev/shm alone never triggers the /tmp sentence.
	m = BuildMemory(1, 6144, 0, 0, 0, []TmpfsInfo{{Path: "/dev/shm", UsedMB: 5000, TotalMB: 9000}}, nil)
	if strings.Contains(m.Note, "/tmp") {
		t.Fatalf("%+v", m)
	}
}

func TestVerdictActive(t *testing.T) {
	m := BuildMemory(6144, 6144, 300, 150, 0, tmp62, nil) // 450 KB/s
	if m.Verdict != "active" || m.Note != "El servidor está paginando: entran 300 KB/s y salen 150 KB/s." {
		t.Fatalf("%+v", m)
	}
	// Boundary: 256 KB/s total is already active; 255 is idle.
	if BuildMemory(1, 2, 128, 128, 0, nil, nil).Verdict != "active" {
		t.Fatal("256 KB/s must be active")
	}
	if BuildMemory(1, 2, 128, 127, 0, nil, nil).Verdict != "idle" {
		t.Fatal("255 KB/s must be idle")
	}
	// The case that used to flicker: 342 KB/s of background swap-in is not paging once it is smoothed away.
	if BuildMemory(1, 2, 100, 0, 0, nil, nil).Verdict != "idle" {
		t.Fatal("100 KB/s must be idle")
	}
	// A memory stall alone makes it active.
	if BuildMemory(1, 2, 0, 0, 1.0, nil, nil).Verdict != "active" {
		t.Fatal("PSI 1 % must be active")
	}
	if BuildMemory(1, 2, 0, 0, 0.99, nil, nil).Verdict != "idle" {
		t.Fatal("PSI 0.99 % must be idle")
	}
}

func TestBuildMemoryCapsAndNeverNil(t *testing.T) {
	var top []SwapProc
	for i := 0; i < 20; i++ {
		top = append(top, SwapProc{PID: i, MB: uint64(100 - i)})
	}
	m := BuildMemory(1, 2, 0, 0, 0, nil, top)
	if len(m.TopSwap) != MaxTopSwap || m.TopSwap[0].PID != 0 {
		t.Fatalf("top = %+v", m.TopSwap)
	}
	b, _ := json.Marshal(BuildMemory(0, 0, 0, 0, 0, nil, nil))
	if !strings.Contains(string(b), `"tmpfs":[]`) || !strings.Contains(string(b), `"topSwap":[]`) {
		t.Fatalf("json = %s", b)
	}
}

func TestHubServesMemory(t *testing.T) {
	h := New(Config{})
	st := h.Snapshot()
	if st.Memory.Verdict != "none" || st.Memory.Tmpfs == nil || st.Memory.TopSwap == nil {
		t.Fatalf("default memory = %+v", st.Memory)
	}
	h.SetMemory(BuildMemory(6144, 6144, 0, 0, 0, tmp62, []SwapProc{{PID: 1, Name: "mariadbd", MB: 2310, Unit: "mariadb"}}))
	b, _ := json.Marshal(h.Snapshot())
	for _, want := range []string{`"memory":{`, `"swapUsedMb":6144`, `"verdict":"idle"`, `"name":"mariadbd"`, `"path":"/tmp"`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("state lacks %s: %s", want, b)
		}
	}
	var nilHub *Hub
	nilHub.SetMemory(MemoryInfo{}) // must not panic
}

func TestVerdictHysteresis(t *testing.T) {
	// Already active: stays active between the exit and the enter threshold...
	if BuildMemoryHyst(1, 2, 150, 0, 0, nil, nil, true).Verdict != "active" {
		t.Fatal("150 KB/s must stay active once active")
	}
	// ...but a fresh verdict at the same value is idle: no flapping around the line.
	if BuildMemoryHyst(1, 2, 150, 0, 0, nil, nil, false).Verdict != "idle" {
		t.Fatal("150 KB/s must not trigger from idle")
	}
	// Clears below the exit bound.
	if BuildMemoryHyst(1, 2, 100, 20, 0.2, nil, nil, true).Verdict != "idle" {
		t.Fatal("120 KB/s and PSI 0.2 must clear")
	}
	// A memory stall keeps it active down to 0.5 %.
	if BuildMemoryHyst(1, 2, 0, 0, 0.6, nil, nil, true).Verdict != "active" {
		t.Fatal("PSI 0.6 must stay active")
	}
}

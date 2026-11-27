package main

import (
	"strings"
	"testing"

	"github.com/dannyjp49/runlight/internal/governor"
)

func TestDemoMemoryIsTheRealIdleCase(t *testing.T) {
	m := demoMemoryInfo(governor.Signals{SwapUsedMB: 6144, SwapTotalMB: 6144, TmpfsUsedMB: 6350, TmpfsTotalMB: 12288, MemSome: 0.2})
	if m.Verdict != "idle" || m.SwapUsedMB != 6144 || m.SwapTotalMB != 6144 || m.SwapInKBs != 0 {
		t.Fatalf("%+v", m)
	}
	if !strings.Contains(m.Note, "6.2 GB") || len(m.Tmpfs) != 2 || m.Tmpfs[0].Path != "/tmp" || m.Tmpfs[0].TotalMB != 12288 {
		t.Fatalf("%+v", m)
	}
	names := map[string]bool{}
	for i, p := range m.TopSwap {
		names[p.Name] = true
		if i > 0 && p.MB > m.TopSwap[i-1].MB {
			t.Fatal("topSwap must be biggest first")
		}
	}
	if !names["mariadbd"] || !names["tienda-main"] || !names["node"] {
		t.Fatalf("topSwap = %+v", m.TopSwap)
	}
}

func TestMockRulesHaveTheNewSwapRules(t *testing.T) {
	var swap, tmp *mockRule
	for i := range mockRules {
		switch mockRules[i].ID {
		case "swap_full":
			swap = &mockRules[i]
		case "tmp_ram":
			tmp = &mockRules[i]
		}
	}
	if swap == nil || tmp == nil {
		t.Fatal("swap_full and tmp_ram must be in the mock catalog")
	}
	if strings.Contains(swap.Description, "paginando en exceso") || !strings.Contains(swap.Description, "paginando") {
		t.Fatalf("swap_full description = %q", swap.Description)
	}
	if tmp.Default != 4 || tmp.Min != 1 || tmp.Max != 64 || tmp.Step != 0.5 || tmp.Unit != "GB" || tmp.Severity != "warning" {
		t.Fatalf("tmp_ram = %+v", *tmp)
	}
}

package telemetry

import "fmt"

// TmpfsInfo is a RAM-backed filesystem (/tmp, /dev/shm).
type TmpfsInfo struct {
	Path    string `json:"path"`
	UsedMB  uint64 `json:"usedMb"`
	TotalMB uint64 `json:"totalMb"`
}

// SwapProc is a process holding swapped-out pages. Unit is a readable
// cgroup/service name, "" when unknown.
type SwapProc struct {
	PID  int    `json:"pid"`
	Name string `json:"name"`
	MB   uint64 `json:"mb"`
	Unit string `json:"unit"`
}

// MemoryInfo explains swap and RAM-backed temp files: how full is swap, is the
// server actually paging, and who is holding it.
type MemoryInfo struct {
	SwapUsedMB  uint64      `json:"swapUsedMb"`
	SwapTotalMB uint64      `json:"swapTotalMb"`
	SwapInKBs   float64     `json:"swapInKbs"`
	SwapOutKBs  float64     `json:"swapOutKbs"`
	Tmpfs       []TmpfsInfo `json:"tmpfs"`
	TopSwap     []SwapProc  `json:"topSwap"` // at most MaxTopSwap, biggest first
	Verdict     string      `json:"verdict"` // "none" | "idle" | "active"
	Note        string      `json:"note"`
}

const (
	// MaxTopSwap caps the "who uses swap" list.
	MaxTopSwap = 8
	// ActiveSwapKBs is the (smoothed) swap traffic, in+out, from which the
	// server counts as paging. It matches the swap_full alert (512 KB/s is
	// hundreds of pages a second); below it, swap traffic is background noise.
	ActiveSwapKBs = 256.0
	// ExitSwapKBs is the lower bound used once the verdict is already
	// "active": hysteresis, so a value hovering around the threshold does not
	// flip the notice on and off.
	ExitSwapKBs = 128.0
	// activePSIMem is the production memory stall (%) from which it counts as
	// paging; exitPSIMem is the hysteresis bound.
	activePSIMem = 1.0
	exitPSIMem   = 0.5
	// tmpNoteMB is the /tmp size from which the idle note mentions it.
	tmpNoteMB = 1024
)

// BuildMemory assembles the memory section and its verdict. psiMem is the
// production-scope memory pressure (some avg10, percent).
//
//	none:   nothing in swap
//	idle:   swap used but (in+out) < 256 KB/s and psiMem < 1 %: stale pages
//	active: otherwise, the server is paging
func BuildMemory(swapUsedMB, swapTotalMB uint64, inKBs, outKBs, psiMem float64, tmpfs []TmpfsInfo, top []SwapProc) MemoryInfo {
	return BuildMemoryHyst(swapUsedMB, swapTotalMB, inKBs, outKBs, psiMem, tmpfs, top, false)
}

// BuildMemoryHyst is BuildMemory with hysteresis: wasActive says the previous
// verdict was "active", which then only clears once traffic falls below
// ExitSwapKBs and the stall below exitPSIMem.
func BuildMemoryHyst(swapUsedMB, swapTotalMB uint64, inKBs, outKBs, psiMem float64, tmpfs []TmpfsInfo, top []SwapProc, wasActive bool) MemoryInfo {
	m := MemoryInfo{
		SwapUsedMB: swapUsedMB, SwapTotalMB: swapTotalMB, SwapInKBs: inKBs, SwapOutKBs: outKBs,
		Tmpfs:   append([]TmpfsInfo{}, tmpfs...),
		TopSwap: append([]SwapProc{}, top...),
	}
	if len(m.TopSwap) > MaxTopSwap {
		m.TopSwap = m.TopSwap[:MaxTopSwap]
	}
	switch {
	case swapUsedMB == 0:
		m.Verdict = "none"
		m.Note = "No hay nada en el swap."
	case !isPaging(inKBs+outKBs, psiMem, wasActive):
		m.Verdict = "idle"
		m.Note = "Swap ocupado pero inactivo: son páginas viejas que nadie usa; no hay impacto."
		for _, t := range m.Tmpfs {
			if t.Path == "/tmp" && t.UsedMB > tmpNoteMB {
				m.Note += fmt.Sprintf(" /tmp (en RAM) guarda %.1f GB de archivos temporales.", float64(t.UsedMB)/1024)
			}
		}
	default:
		m.Verdict = "active"
		m.Note = fmt.Sprintf("El servidor está paginando: entran %.0f KB/s y salen %.0f KB/s.", inKBs, outKBs)
	}
	return m
}

func isPaging(kbs, psiMem float64, wasActive bool) bool {
	if wasActive {
		return kbs >= ExitSwapKBs || psiMem >= exitPSIMem
	}
	return kbs >= ActiveSwapKBs || psiMem >= activePSIMem
}

// SetMemory stores the latest memory section for the dashboard.
func (h *Hub) SetMemory(m MemoryInfo) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.memory = m
	h.mu.Unlock()
}

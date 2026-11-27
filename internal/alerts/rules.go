package alerts

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const alertsURL = "/#/alertas"

// ruleSpec is the static definition of one rule: metadata for the UI plus the
// timing semantics the engine applies generically.
type ruleSpec struct {
	id       RuleID
	sev      Severity
	title    string
	desc     string
	unit     string
	label    string
	def      float64
	min, max float64
	step     float64

	forDur     time.Duration // condition must hold this long before alerting
	clearDur   time.Duration // clear condition must hold this long before resolving
	repeat     time.Duration // 0 = never repeat
	noResolved bool          // do not send a "resolved" notification

	eval func(e *Engine, th float64, in Inputs) []check
}

// check is the per-key verdict of a rule for one tick.
type check struct {
	key     string
	title   string
	unknown bool          // not enough data: leave state untouched
	forOver time.Duration // overrides the rule's fixed hold time when > 0
	fire    bool
	clear   bool
	body    string // text while the alert is active
	okBody  string // text of the "resolved" notification
}

var specs = []ruleSpec{
	{
		id: DiskLow, sev: Critical,
		title: "Poco espacio en disco",
		desc:  "Avisa cuando el disco libre baja del umbral. runlight deja de arrancar runners para no llenar el disco.",
		unit:  "% libre",
		label: "Avisar si queda menos de",
		def:   12, min: 3, max: 50, step: 1,
		forDur: 60 * time.Second, clearDur: 60 * time.Second, repeat: 30 * time.Minute,
		eval: evalDiskLow,
	},
	{
		id: SwapFull, sev: Warning,
		title: "Servidor paginando a swap",
		desc:  "Avisa cuando el swap está casi lleno Y el servidor está paginando de verdad (o casi sin memoria). Un swap lleno pero quieto no avisa: son páginas viejas sin impacto.",
		unit:  "% usado",
		label: "Avisar si el swap supera (y hay actividad)",
		def:   95, min: 50, max: 100, step: 1,
		forDur: 120 * time.Second, repeat: 12 * time.Hour,
		eval: evalSwapFull,
	},
	{
		id: TmpRAM, sev: Warning,
		title: "/tmp ocupa mucha RAM",
		desc:  "Avisa cuando /tmp es un tmpfs (vive en RAM) y guarda más archivos temporales que el umbral: acaban en swap.",
		unit:  "GB",
		label: "Avisar si /tmp en RAM supera",
		def:   4, min: 1, max: 64, step: 0.5,
		forDur: 300 * time.Second, repeat: 6 * time.Hour,
		eval: evalTmpRAM,
	},
	{
		id: MemLow, sev: Warning,
		title: "Poca memoria disponible",
		desc:  "Avisa cuando la memoria disponible cae cerca de la reserva intocable (se mide en veces la reserva).",
		unit:  "× reserva",
		label: "Avisar si queda menos de",
		def:   1.5, min: 1, max: 4, step: 0.1,
		forDur: 120 * time.Second, repeat: 6 * time.Hour,
		eval: evalMemLow,
	},
	{
		id: QueueStuck, sev: Warning,
		title: "Cola de jobs atascada",
		desc:  "Avisa cuando el job más antiguo lleva demasiado esperando un runner, y explica la causa.",
		unit:  "min",
		label: "Avisar si un job espera más de",
		def:   15, min: 2, max: 240, step: 1,
		forDur: 60 * time.Second, repeat: 6 * time.Hour,
		eval: evalQueueStuck,
	},
	{
		id: JobFailures, sev: Warning,
		title: "Fallos repetidos",
		desc:  "Avisa cuando un mismo repositorio acumula fallos de jobs en 30 minutos. Una alerta por repositorio.",
		unit:  "fallos",
		label: "Avisar a partir de",
		def:   3, min: 2, max: 20, step: 1,
		clearDur: 0, repeat: 0,
		eval: evalJobFailures,
	},
	{
		id: ProdPressure, sev: Warning,
		title: "Producción bajo presión",
		desc:  "Avisa cuando la CPU de producción está al límite de forma sostenida y runlight tiene que frenar.",
		unit:  "min",
		label: "Avisar si dura más de",
		def:   3, min: 1, max: 60, step: 1,
		clearDur: 120 * time.Second, repeat: 6 * time.Hour,
		eval: evalProdPressure,
	},
	{
		id: DaemonRestart, sev: Critical,
		title:      "runlight se reinició sin avisar",
		desc:       "Avisa al instante, una sola vez, cuando el servicio arranca tras un reinicio no limpio (caída o corte).",
		noResolved: true,
		eval:       evalDaemonRestart,
	},
	{
		id: JanitorStuck, sev: Warning,
		title: "El limpiador no libera espacio",
		desc:  "Avisa cuando el disco sigue bajo y el limpiador de cachés pasa varias veces sin liberar nada: hay que actuar a mano.",
		unit:  "% libre",
		label: "Avisar si queda menos de",
		def:   12, min: 3, max: 50, step: 1,
		forDur: 600 * time.Second, clearDur: 120 * time.Second, repeat: 6 * time.Hour,
		eval: evalJanitorStuck,
	},
}

func specByID(id RuleID) (ruleSpec, bool) {
	for _, s := range specs {
		if s.id == id {
			return s, true
		}
	}
	return ruleSpec{}, false
}

func ruleOrder(id RuleID) int {
	for i, s := range specs {
		if s.id == id {
			return i
		}
	}
	return len(specs)
}

// ---- formatting helpers ----

func fmtGB(mb uint64) string {
	v := float64(mb) / 1024
	if v >= 100 {
		return fmt.Sprintf("%.0f GB", v)
	}
	return fmt.Sprintf("%.1f GB", v)
}

func fmtPct(v float64) string { return fmt.Sprintf("%.1f %%", v) }

func fmtDur(sec int) string {
	if sec < 0 {
		sec = 0
	}
	if sec < 90 {
		return fmt.Sprintf("%d s", sec)
	}
	m := (sec + 30) / 60
	if m < 120 {
		return fmt.Sprintf("%d min", m)
	}
	h, r := m/60, m%60
	if r == 0 {
		return fmt.Sprintf("%d h", h)
	}
	return fmt.Sprintf("%d h %d min", h, r)
}

func fmtNum(v float64) string {
	if v == math.Trunc(v) {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.1f", v)
}

func diskPct(in Inputs) (float64, bool) {
	if in.DiskTotalMB == 0 {
		return 0, false
	}
	return float64(in.DiskFreeMB) / float64(in.DiskTotalMB) * 100, true
}

// ---- rule evaluators ----

func evalDiskLow(e *Engine, th float64, in Inputs) []check {
	p, ok := diskPct(in)
	if !ok {
		return []check{{key: string(DiskLow), unknown: true}}
	}
	tail := "Libera espacio o baja los topes de caché."
	if in.GovernorReason == "low-disk" {
		tail = "runlight dejó de arrancar runners."
	}
	return []check{{
		key:   string(DiskLow),
		title: "Poco espacio en disco",
		fire:  p < th,
		clear: p > th+3,
		body: fmt.Sprintf("Quedan %s libres de %s (%s). %s",
			fmtGB(in.DiskFreeMB), fmtGB(in.DiskTotalMB), fmtPct(p), tail),
		okBody: fmt.Sprintf("El disco vuelve a tener %s libres de %s (%s).",
			fmtGB(in.DiskFreeMB), fmtGB(in.DiskTotalMB), fmtPct(p)),
	}}
}

// swapActiveKBs is the sustained swap traffic (in+out) that counts as paging.
const swapActiveKBs = 512.0

func evalSwapFull(e *Engine, th float64, in Inputs) []check {
	if in.SwapTotalMB == 0 {
		return []check{{key: string(SwapFull), unknown: true}}
	}
	p := float64(in.SwapUsedMB) / float64(in.SwapTotalMB) * 100
	act := in.SwapInKBs + in.SwapOutKBs
	memLimit := float64(in.MemReserveMB) * 1.5
	lowMem := in.MemReserveMB > 0 && float64(in.MemAvailMB) < memLimit
	paging := act >= swapActiveKBs
	// Resolve when the swap drained, or when it is no longer being used and
	// memory recovered (half the trigger traffic, 20 % above the memory line).
	calm := act < swapActiveKBs/2 && (in.MemReserveMB == 0 || float64(in.MemAvailMB) >= memLimit*1.2)
	var why string
	switch {
	case paging && lowMem:
		why = fmt.Sprintf("Está paginando: entran %.0f KB/s y salen %.0f KB/s, y quedan solo %s de memoria disponible.",
			in.SwapInKBs, in.SwapOutKBs, fmtGB(in.MemAvailMB))
	case paging:
		why = fmt.Sprintf("Está paginando: entran %.0f KB/s y salen %.0f KB/s.", in.SwapInKBs, in.SwapOutKBs)
	default:
		why = fmt.Sprintf("Queda poca memoria disponible (%s) y el swap está casi lleno.", fmtGB(in.MemAvailMB))
	}
	return []check{{
		key:   string(SwapFull),
		title: "Servidor paginando a swap",
		fire:  p >= th && (paging || lowMem),
		clear: p < th-10 || calm,
		body:  fmt.Sprintf("El swap está al %s (%s de %s). %s", fmtPct(p), fmtGB(in.SwapUsedMB), fmtGB(in.SwapTotalMB), why),
		okBody: fmt.Sprintf("El swap dejó de ser un problema: %s (%s de %s) y ya no pagina.",
			fmtPct(p), fmtGB(in.SwapUsedMB), fmtGB(in.SwapTotalMB)),
	}}
}

func evalTmpRAM(e *Engine, th float64, in Inputs) []check {
	usedGB := float64(in.TmpfsUsedMB) / 1024
	// TmpfsTotalMB == 0 means /tmp is not a tmpfs: nothing in RAM, so it
	// never fires and any active alert resolves.
	on := in.TmpfsTotalMB > 0
	pct := 0.0
	if on {
		pct = float64(in.TmpfsUsedMB) / float64(in.TmpfsTotalMB) * 100
	}
	return []check{{
		key:   string(TmpRAM),
		title: "/tmp ocupa mucha RAM",
		fire:  on && usedGB >= th,
		clear: !on || usedGB < th*0.8,
		body: fmt.Sprintf("/tmp ocupa %s de RAM (%s de su límite). Los archivos temporales viejos acaban en swap. Activa la limpieza de /tmp o usa TMPDIR en disco.",
			fmtGB(in.TmpfsUsedMB), fmtPct(pct)),
		okBody: fmt.Sprintf("/tmp bajó a %s de RAM.", fmtGB(in.TmpfsUsedMB)),
	}}
}

func evalMemLow(e *Engine, th float64, in Inputs) []check {
	avail := float64(in.MemAvailMB)
	limit := float64(in.MemReserveMB) * th
	return []check{{
		key:   string(MemLow),
		title: "Poca memoria disponible",
		fire:  in.MemReserveMB > 0 && avail < limit,
		clear: avail > limit*1.2,
		body: fmt.Sprintf("Quedan %s de memoria disponible; la reserva es de %s y el aviso salta por debajo de %s.",
			fmtGB(in.MemAvailMB), fmtGB(in.MemReserveMB), fmtGB(uint64(limit))),
		okBody: fmt.Sprintf("La memoria disponible se recuperó: %s (reserva %s).", fmtGB(in.MemAvailMB), fmtGB(in.MemReserveMB)),
	}}
}

func queueCause(in Inputs) string {
	switch in.GovernorReason {
	case "pressure":
		return "Producción está bajo presión y runlight frena los runners."
	case "low-memory":
		return "Falta memoria y runlight no arranca más runners."
	case "low-disk":
		return "Falta espacio en disco y runlight no arranca más runners."
	case "no-headroom":
		return "No hay margen de memoria para otro runner."
	}
	switch {
	case in.Runners == 0:
		return "No hay ningún runner activo."
	case in.Limit > 0 && in.Runners >= in.Limit && in.Busy >= in.Runners:
		return fmt.Sprintf("Los %d runners permitidos están ocupados.", in.Runners)
	case in.Busy < in.Runners:
		return fmt.Sprintf("Hay %d de %d runners libres pero la cola no avanza; revisa las etiquetas de los jobs.", in.Runners-in.Busy, in.Runners)
	}
	return fmt.Sprintf("Hay %d de %d runners ocupados.", in.Busy, in.Runners)
}

func evalQueueStuck(e *Engine, th float64, in Inputs) []check {
	limit := th * 60
	wait := float64(in.QueueOldestWaitSec)
	return []check{{
		key:   string(QueueStuck),
		title: "Cola de jobs atascada",
		fire:  in.QueueDepth > 0 && wait > limit,
		clear: in.QueueDepth == 0 || wait < limit*0.5,
		body: fmt.Sprintf("Hay %d %s en cola y el más antiguo lleva %s esperando. %s",
			in.QueueDepth, plural(in.QueueDepth, "job", "jobs"), fmtDur(in.QueueOldestWaitSec), queueCause(in)),
		okBody: "La cola volvió a moverse.",
	}}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

const (
	failWindow = 30 * time.Minute
	failQuiet  = 60 * time.Minute
)

func evalJobFailures(e *Engine, th float64, in Inputs) []check {
	type agg struct {
		count int
		jobs  []string
		last  time.Time
	}
	seen := map[FailedJob]bool{}
	byRepo := map[string]*agg{}
	for _, f := range in.FailedJobs {
		if f.At.After(in.Now) {
			continue
		}
		if f.Repo == "" {
			f.Repo = "desconocido"
		}
		if seen[f] {
			continue
		}
		seen[f] = true
		a := byRepo[f.Repo]
		if a == nil {
			a = &agg{}
			byRepo[f.Repo] = a
		}
		if f.At.After(a.last) {
			a.last = f.At
		}
		if in.Now.Sub(f.At) <= failWindow {
			a.count++
			if f.Job != "" && !containsStr(a.jobs, f.Job) {
				a.jobs = append(a.jobs, f.Job)
			}
		}
	}
	repos := map[string]bool{}
	for r := range byRepo {
		repos[r] = true
	}
	for key, a := range e.active {
		if a.Rule == JobFailures {
			repos[strings.TrimPrefix(key, string(JobFailures)+":")] = true
		}
	}
	names := make([]string, 0, len(repos))
	for r := range repos {
		names = append(names, r)
	}
	sort.Strings(names)

	// Forget quiet repos that never became alerts so the map stays bounded.
	for key, last := range e.lastFail {
		if e.active[key] == nil && in.Now.Sub(last) >= failQuiet {
			delete(e.lastFail, key)
		}
	}

	out := make([]check, 0, len(names))
	for _, repo := range names {
		key := string(JobFailures) + ":" + repo
		a := byRepo[repo]
		if a == nil {
			a = &agg{}
		}
		if a.last.After(e.lastFail[key]) {
			e.lastFail[key] = a.last
		}
		last := e.lastFail[key]
		quiet := !last.IsZero() && in.Now.Sub(last) >= failQuiet
		jobs := ""
		if len(a.jobs) > 0 {
			sort.Strings(a.jobs)
			jobs = " (" + strings.Join(limitList(a.jobs, 4), ", ") + ")"
		}
		out = append(out, check{
			key:   key,
			title: "Fallos repetidos en " + repo,
			fire:  float64(a.count) >= th,
			clear: quiet,
			body: fmt.Sprintf("%d %s han fallado en %s en los últimos 30 min%s.",
				a.count, plural(a.count, "job", "jobs"), repo, jobs),
			okBody: fmt.Sprintf("%s lleva 60 min sin fallos nuevos.", repo),
		})
	}
	return out
}

func containsStr(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func limitList(l []string, n int) []string {
	if len(l) <= n {
		return l
	}
	out := append([]string{}, l[:n]...)
	return append(out, fmt.Sprintf("y %d más", len(l)-n))
}

func evalProdPressure(e *Engine, th float64, in Inputs) []check {
	if in.ProdCPULimit <= 0 {
		return []check{{key: string(ProdPressure), unknown: true}}
	}
	return []check{{
		key:     string(ProdPressure),
		forOver: time.Duration(th * float64(time.Minute)),
		title:   "Producción bajo presión",
		fire:    in.ProdCPUPressure >= in.ProdCPULimit,
		clear:   in.ProdCPUPressure < in.ProdCPULimit*0.8,
		body: fmt.Sprintf("La CPU de producción está bajo presión: %s (límite %s) durante más de %s min. runlight frena los runners para protegerla.",
			fmtPct(in.ProdCPUPressure), fmtPct(in.ProdCPULimit), fmtNum(th)),
		okBody: fmt.Sprintf("La presión de CPU de producción bajó a %s (límite %s).", fmtPct(in.ProdCPUPressure), fmtPct(in.ProdCPULimit)),
	}}
}

const restartTTL = 60 * time.Minute

func evalDaemonRestart(e *Engine, th float64, in Inputs) []check {
	c := check{
		key:   string(DaemonRestart),
		title: "runlight se reinició sin avisar",
		fire:  in.UnexpectedRestart,
		body:  "runlight arrancó tras un cierre no limpio (caída, corte o reinicio del servidor). Revisa los registros si se repite.",
	}
	if a := e.active[c.key]; a != nil {
		c.clear = in.Now.Sub(a.Since) >= restartTTL
	}
	return []check{c}
}

func evalJanitorStuck(e *Engine, th float64, in Inputs) []check {
	p, ok := diskPct(in)
	if !ok {
		return []check{{key: string(JanitorStuck), unknown: true}}
	}
	cond := p < th && in.JanitorRuns >= 2 && in.JanitorFreedMBLast == 0
	return []check{{
		key:   string(JanitorStuck),
		title: "El limpiador no libera espacio",
		fire:  cond,
		clear: !cond,
		body: fmt.Sprintf("El disco tiene %s libres de %s (%s) y el limpiador ya pasó %d veces sin liberar nada. Hay que liberar espacio a mano.",
			fmtGB(in.DiskFreeMB), fmtGB(in.DiskTotalMB), fmtPct(p), in.JanitorRuns),
		okBody: "El limpiador volvió a liberar espacio o el disco se recuperó.",
	}}
}

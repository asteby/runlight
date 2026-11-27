package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dannyjp49/runlight/internal/alerts"
	"github.com/dannyjp49/runlight/internal/governor"
	"github.com/dannyjp49/runlight/internal/janitor"
	"github.com/dannyjp49/runlight/internal/push"
	"github.com/dannyjp49/runlight/internal/scaler"
	"github.com/dannyjp49/runlight/internal/settings"
	"github.com/dannyjp49/runlight/internal/telemetry"
)

const runningMarker = "running"

// markRunning creates <dir>/running and reports whether it already existed,
// i.e. whether the previous process died without a clean shutdown. The
// returned func removes the marker; call it only on a clean exit.
func markRunning(dir string) (unexpected bool, clear func(), err error) {
	path := filepath.Join(dir, runningMarker)
	if _, statErr := os.Stat(path); statErr == nil {
		unexpected = true
	}
	body := fmt.Sprintf("pid=%d started=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return unexpected, func() {}, err
	}
	return unexpected, func() { _ = os.Remove(path) }, nil
}

// prepareStateDir creates dir (0700) when it is set. It returns "" and a
// reason when persistence must stay disabled.
func prepareStateDir(dir string) (string, string) {
	if dir == "" {
		return "", "--state-dir vacío"
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err.Error()
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return "", "no es un directorio utilizable"
	}
	return dir, ""
}

// ---- settings schema and live application ----

const gib = 1 << 30

func clampF(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

func clampI(v, lo, hi int) int { return max(lo, min(hi, v)) }

// settingDefaults turns the effective startup values into schema defaults,
// kept inside the editable ranges so a flag outside them cannot break Open.
func settingDefaults(c config, maxRunners int, gp governor.Params) settings.Defaults {
	return settings.Defaults{
		MaxRunners:         clampI(maxRunners, 1, 16),
		MinRunners:         clampI(c.min, 0, 8),
		MemPerRunnerMB:     clampI(c.memPerRun, 256, 16384),
		MemReserveMB:       clampI(c.memReserve, 256, 16384),
		DiskMinFreeMB:      clampI(c.diskMin, 1024, 102400),
		IdleTTLSec:         clampI(int(c.idleTTL.Seconds()), 10, 3600),
		JanitorIntervalMin: clampI(int(c.janitorEvery.Minutes()), 1, 240),
		PSICPU:             clampF(gp.CPUHigh, 5, 95),
		PSIMem:             clampF(gp.MemHigh, 1, 50),
		PSIIO:              clampF(gp.IOHigh, 5, 95),
	}
}

func cacheCaps(rules []janitor.Rule) map[string]float64 {
	caps := map[string]float64{}
	for _, r := range rules {
		caps[r.Name] = clampF(float64(r.MaxBytes)/gib, 0.5, 200)
	}
	return caps
}

func asFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	}
	return 0, false
}

func numOr(values map[string]any, key string, def float64) float64 {
	if f, ok := asFloat(values[key]); ok {
		return f
	}
	return def
}

// validateSettings holds the cross-field rules the schema cannot express.
func validateSettings(v map[string]any) settings.FieldErrors {
	errs := settings.FieldErrors{}
	minR, okMin := asFloat(v["scaling.minRunners"])
	maxR, okMax := asFloat(v["scaling.maxRunners"])
	if okMin && okMax && minR > maxR {
		errs["scaling.minRunners"] = "No puede ser mayor que el máximo de runners."
	}
	if r, ok := asFloat(v["scaling.memReserveMb"]); ok && r < 256 {
		errs["scaling.memReserveMb"] = "La reserva debe ser de al menos 256 MB."
	}
	if len(errs) == 0 {
		return nil
	}
	return errs
}

// alertConfigFrom reads "alert.<rule>.enabled/threshold" over the defaults.
func alertConfigFrom(values map[string]any) alerts.Config {
	cfg := alerts.DefaultConfig()
	for id, rc := range cfg {
		if b, ok := values["alert."+string(id)+".enabled"].(bool); ok {
			rc.Enabled = b
		}
		rc.Threshold = numOr(values, "alert."+string(id)+".threshold", rc.Threshold)
		cfg[id] = rc
	}
	return cfg
}

// live applies a full set of setting values to the running components. It is
// idempotent, so OnChange simply hands it everything.
type live struct {
	gov *governor.Governor
	sc  *scaler.Scaler
	jan *janitor.Janitor // nil without caches
	hub *telemetry.Hub
	eng *alerts.Engine
	col *collector
	log *slog.Logger

	baseRules []janitor.Rule

	mu   sync.Mutex
	tcfg telemetry.Config
	// latest, when set, returns the current stored values. OnChange hooks of
	// concurrent updates can arrive out of order; apply re-reads under mu so the
	// last writer to run always applies the newest state.
	latest func() map[string]any

	// Read by the control loop every tick.
	memReserveMB atomic.Uint64
	cpuHighBits  atomic.Uint64
}

func (l *live) cpuHigh() float64 { return math.Float64frombits(l.cpuHighBits.Load()) }

func (l *live) apply(v map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.latest != nil {
		v = l.latest()
	}
	maxR := int(numOr(v, "scaling.maxRunners", float64(l.tcfg.Max)))
	minR := int(numOr(v, "scaling.minRunners", float64(l.tcfg.Min)))
	memPer := uint64(numOr(v, "scaling.memPerRunnerMb", 1024))
	memRes := uint64(numOr(v, "scaling.memReserveMb", 1024))
	diskMin := uint64(numOr(v, "scaling.diskMinFreeMb", 4096))
	idle := time.Duration(numOr(v, "scaling.idleTtlSec", 30)) * time.Second
	cpuHigh := numOr(v, "pressure.cpuHigh", 60)
	memHigh := numOr(v, "pressure.memHigh", 8)
	ioHigh := numOr(v, "pressure.ioHigh", 50)

	l.gov.UpdateParams(func(p *governor.Params) {
		p.Min, p.Max = minR, maxR
		p.MemPerRunnerMB, p.MemReserveMB, p.DiskMinFreeMB = memPer, memRes, diskMin
		p.CPUHigh, p.MemHigh, p.IOHigh = cpuHigh, memHigh, ioHigh
	})
	l.col.defMemMB.Store(memPer)
	l.memReserveMB.Store(memRes)
	l.cpuHighBits.Store(math.Float64bits(cpuHigh))
	l.sc.SetMinRunners(minR)
	l.sc.SetIdleTTL(idle)
	l.sc.SetSlots(maxR)

	if l.jan != nil {
		if mins := numOr(v, "janitor.intervalMin", 0); mins >= 1 {
			l.jan.SetInterval(time.Duration(mins) * time.Minute)
		}
		rules := append([]janitor.Rule(nil), l.baseRules...)
		for i, r := range rules {
			if gb, ok := asFloat(v["cache."+r.Name+".capGb"]); ok && math.Abs(gb-clampF(float64(r.MaxBytes)/gib, 0.5, 200)) > 1e-9 {
				rules[i].MaxBytes = int64(gb * gib)
			}
		}
		l.jan.SetRules(rules)
	}

	l.eng.SetConfig(alertConfigFrom(v))

	l.tcfg.Min, l.tcfg.Max, l.tcfg.IdleTTLSec = minR, maxR, int(idle.Seconds())
	cfg := l.tcfg
	l.hub.SetConfig(cfg)
}

// ---- alerts -> push + telemetry ----

func pushLevel(s alerts.Severity) string {
	switch s {
	case alerts.Critical:
		return "critical"
	case alerts.Warning:
		return "warning"
	}
	return "info"
}

// newAlertSink records every alert in the decision log and, when push is
// available, delivers it from a background worker so a slow push service
// never stalls the control loop.
func newAlertSink(ctx context.Context, ps *push.Store, hub *telemetry.Hub, log *slog.Logger) alerts.Sink {
	queue := make(chan push.Notification, 64)
	if ps != nil {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case n := <-queue:
					rep := ps.Send(ctx, n)
					if rep.Failed > 0 || rep.Removed > 0 {
						log.Warn("push delivery", "sent", rep.Sent, "failed", rep.Failed, "removed", rep.Removed)
					}
				}
			}
		}()
	}
	return func(_ context.Context, n alerts.Notification) {
		hub.Eventf("alert", "%s: %s", n.Title, n.Body)
		if ps == nil {
			return
		}
		pn := push.Notification{
			Title: n.Title, Body: n.Body, Tag: n.Tag, URL: n.URL,
			Level: pushLevel(n.Severity), Renotify: !n.Resolved,
		}
		select {
		case queue <- pn:
		default:
			log.Warn("push queue full, notification dropped", "tag", n.Tag)
		}
	}
}

// ---- operator actions ----

type actions struct {
	sc  *scaler.Scaler
	jan *janitor.Janitor
	hub *telemetry.Hub
}

func (a *actions) Pause() (string, error) {
	a.sc.SetPaused(true)
	a.hub.SetPaused(true)
	a.hub.Event("info", "Escalado en pausa desde el panel: no se arrancarán runners nuevos.")
	return "En pausa: no arrancará runners nuevos. Los jobs en marcha terminan con normalidad.", nil
}

func (a *actions) Resume() (string, error) {
	a.sc.SetPaused(false)
	a.hub.SetPaused(false)
	a.hub.Event("info", "Escalado reanudado desde el panel.")
	return "Reanudado: vuelve a arrancar runners cuando haya trabajo.", nil
}

func (a *actions) JanitorNow() (string, error) {
	if a.jan == nil {
		return "", errors.New("No hay cachés configuradas que limpiar")
	}
	a.jan.RunNow()
	a.hub.Event("info", "Limpieza de cachés pedida desde el panel.")
	return "Limpieza de cachés en marcha. El resultado aparece en el registro.", nil
}

// ---- alert inputs ----

// alertInputs builds the engine's inputs for one control tick.
func alertInputs(now time.Time, sig governor.Signals, hub *telemetry.Hub, gov *governor.Governor,
	lv *live, total, busy, limit int, restart bool) alerts.Inputs {
	js := hub.JanitorStats()
	in := alerts.Inputs{
		Now:        now,
		DiskFreeMB: sig.DiskFreeMB, DiskTotalMB: sig.DiskTotalMB,
		MemAvailMB: sig.MemAvailMB, MemReserveMB: lv.memReserveMB.Load(),
		SwapUsedMB: sig.SwapUsedMB, SwapTotalMB: sig.SwapTotalMB,
		SwapInKBs: sig.SwapInKBs, SwapOutKBs: sig.SwapOutKBs,
		TmpfsUsedMB: sig.TmpfsUsedMB, TmpfsTotalMB: sig.TmpfsTotalMB,
		Runners: total, Busy: busy, Limit: limit,
		GovernorReason: string(gov.Reason()),
		QueueDepth:     hub.QueueDepth(), QueueOldestWaitSec: hub.QueueOldestWaitSec(now),
		ProdCPUPressure: sig.CPUSome, ProdCPULimit: lv.cpuHigh(),
		UnexpectedRestart: restart,
		JanitorRuns:       js.Runs, JanitorFreedMBLast: js.LastFreed,
	}
	for _, f := range hub.RecentFailures(now.Add(-2 * time.Hour)) {
		in.FailedJobs = append(in.FailedJobs, alerts.FailedJob{Repo: f.Repo, Job: f.Job, At: f.At})
	}
	return in
}

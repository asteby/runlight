// Command runlight is a tiny autoscaler for GitHub Actions self-hosted
// runners on a single small Linux VPS.
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"

	"github.com/dannyjp49/runlight/internal/alerts"
	"github.com/dannyjp49/runlight/internal/dashboard"
	"github.com/dannyjp49/runlight/internal/governor"
	"github.com/dannyjp49/runlight/internal/janitor"
	"github.com/dannyjp49/runlight/internal/push"
	"github.com/dannyjp49/runlight/internal/scaler"
	"github.com/dannyjp49/runlight/internal/settings"
	"github.com/dannyjp49/runlight/internal/sysmon"
	"github.com/dannyjp49/runlight/internal/telemetry"
)

var version = "dev"

type config struct {
	url          string
	name         string
	labels       string
	group        string
	min, max     int
	memPerRun    int
	image        string
	idleTTL      time.Duration
	tick         time.Duration
	logLevel     string
	logJSON      bool
	backendName  string
	templateDir  string
	jobsDir      string
	runAs        string
	nice         int
	runtimeMax   time.Duration
	envVars      multiFlag
	cpusPerRun   float64
	cgroup       string
	deleteOnExit bool
	psiCgroup    string
	psiCPU       float64
	psiMem       float64
	psiIO        float64
	memReserve   int
	diskMin      int
	listen       string
	caches       multiFlag
	ephemerals   multiFlag
	janitorEvery time.Duration
	janitorDry   bool
	dashUser     string
	hashPassword bool
	showVersion  bool
	stateDir     string
	vapidSubject string
}

func main() {
	var c config
	flag.StringVar(&c.url, "url", os.Getenv("RUNLIGHT_URL"), "repo/org/enterprise URL to serve (env RUNLIGHT_URL)")
	flag.StringVar(&c.name, "name", "runlight", "scale set name; use it in `runs-on:`")
	flag.StringVar(&c.labels, "labels", "", "extra comma-separated labels")
	flag.StringVar(&c.group, "runner-group", scaleset.DefaultRunnerGroup, "runner group")
	flag.IntVar(&c.min, "min", 0, "warm runners kept alive (0 = scale to zero)")
	flag.IntVar(&c.max, "max", 0, "hard ceiling of runners (0 = auto from CPU/RAM)")
	flag.IntVar(&c.memPerRun, "mem-per-runner-mb", 1024, "memory budget per runner, in MB")
	flag.StringVar(&c.backendName, "backend", "docker", "docker (isolated containers) | systemd (native on host, for deploy jobs)")
	flag.StringVar(&c.templateDir, "template-dir", "", "systemd backend: extracted actions/runner directory")
	flag.StringVar(&c.jobsDir, "jobs-dir", "", "systemd backend: where per-job runner copies live")
	flag.StringVar(&c.runAs, "run-as", "ubuntu", "systemd backend: unix user jobs run as")
	flag.IntVar(&c.nice, "nice", 10, "systemd backend: CPU niceness of jobs")
	flag.DurationVar(&c.runtimeMax, "runtime-max", 90*time.Minute, "systemd backend: kill jobs running longer than this (0 = never)")
	flag.Var(&c.envVars, "env", "systemd backend: KEY=VALUE injected into every job (repeatable)")
	flag.Float64Var(&c.cpusPerRun, "cpus-per-runner", 0, "CPU cap per runner container (0 = cores/max, so jobs cannot starve the host)")
	flag.StringVar(&c.cgroup, "cgroup-parent", "", "systemd slice for runner containers, e.g. runlight.slice")
	flag.BoolVar(&c.deleteOnExit, "delete-on-exit", false, "delete the scale set on shutdown (default: keep it so restarts resume seamlessly)")
	flag.IntVar(&c.memReserve, "mem-reserve-mb", 1024, "memory that must always stay available for everyone else; below it no runner is started")
	flag.IntVar(&c.diskMin, "disk-min-free-mb", 4096, "stop starting runners when free disk drops below this")
	flag.StringVar(&c.listen, "listen", "127.0.0.1:9128", "address of the read-only dashboard (empty = disabled). Put it behind an authenticating proxy before exposing it")
	flag.Var(&c.caches, "cache", "capped cache to keep in check: name=dir:capGB[:lru|idle[:minAgeHours]] (repeatable)")
	flag.Var(&c.ephemerals, "cache-ephemeral", "remove per-runner dirs of runners that no longer exist: root:prefix1,prefix2[:minAgeMinutes] (repeatable)")
	flag.DurationVar(&c.janitorEvery, "janitor-interval", 10*time.Minute, "how often the cache janitor runs (it also runs sooner when the disk is low)")
	flag.BoolVar(&c.janitorDry, "janitor-dry-run", false, "report what the janitor would delete without deleting anything")
	flag.StringVar(&c.dashUser, "dash-user", os.Getenv("RUNLIGHT_DASH_USER"), "user of the dashboard login screen; the password hash comes from env RUNLIGHT_DASH_PASSWORD_HASH. Empty = no login (keep it behind a proxy)")
	flag.BoolVar(&c.hashPassword, "hash-password", false, "read a password from stdin, print its hash for RUNLIGHT_DASH_PASSWORD_HASH and exit")
	flag.StringVar(&c.psiCgroup, "psi-cgroup", "", "measure pressure of this cgroup (e.g. /sys/fs/cgroup/system.slice = production) instead of the whole host")
	flag.Float64Var(&c.psiCPU, "psi-cpu-high", 0, "CPU stall % (avg10) that triggers a back-off (0 = default 60)")
	flag.Float64Var(&c.psiMem, "psi-mem-high", 0, "memory stall % (avg10) that triggers a back-off (0 = default 8)")
	flag.Float64Var(&c.psiIO, "psi-io-high", 0, "IO stall % (avg10) that triggers a back-off (0 = default 50)")
	flag.StringVar(&c.image, "image", "ghcr.io/actions/actions-runner:latest", "runner container image")
	flag.DurationVar(&c.idleTTL, "idle-ttl", 30*time.Second, "reap idle runners above --min after this long")
	flag.DurationVar(&c.tick, "tick", 2*time.Second, "host sampling interval")
	flag.StringVar(&c.logLevel, "log-level", "info", "debug|info|warn|error")
	flag.BoolVar(&c.logJSON, "log-json", false, "JSON logs")
	flag.StringVar(&c.stateDir, "state-dir", "/var/lib/runlight", "where runlight keeps push subscriptions, VAPID keys and edited settings (empty = push and settings disabled)")
	flag.StringVar(&c.vapidSubject, "vapid-subject", "https://runlight.asteby.com", "VAPID subject (contact URL or mailto:) sent to push services")
	flag.BoolVar(&c.showVersion, "version", false, "print version and exit")
	flag.Parse()

	if c.hashPassword {
		if err := printPasswordHash(); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	if c.showVersion {
		fmt.Println("runlight", version)
		return
	}
	log := newLogger(c)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, c, log); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, c config, log *slog.Logger) error {
	if c.url == "" {
		return errors.New("--url is required")
	}
	client, err := newClient(c.url)
	if err != nil {
		return err
	}

	mon := newSampler(c.psiCgroup)
	first, _ := mon.Sample()
	maxRunners := c.max
	if maxRunners <= 0 {
		maxRunners = autoMax(first, c.memPerRun)
	}
	// Start from the safe defaults: building Params by hand once left the
	// memory reserve and the disk guard at zero, i.e. switched off.
	gp := governor.Defaults()
	gp.Min, gp.Max, gp.MemPerRunnerMB = c.min, maxRunners, uint64(c.memPerRun)
	gp.MemReserveMB, gp.DiskMinFreeMB = uint64(c.memReserve), uint64(c.diskMin)
	if c.psiCPU > 0 {
		gp.CPUHigh = c.psiCPU
	}
	if c.psiMem > 0 {
		gp.MemHigh = c.psiMem
	}
	if c.psiIO > 0 {
		gp.IOHigh = c.psiIO
	}
	gov := governor.New(gp)
	log.Info("runlight starting", "version", version, "max", maxRunners, "min", c.min,
		"psi", first.HasPSI, "mem_avail_mb", first.MemAvailMB)

	groupID := 1
	if c.group != scaleset.DefaultRunnerGroup {
		g, err := client.GetRunnerGroupByName(ctx, c.group)
		if err != nil {
			return fmt.Errorf("runner group: %w", err)
		}
		groupID = g.ID
	}
	// Get-or-create: after a crash or restart we resume the same scale set, so
	// queued jobs stay assigned instead of being orphaned.
	set, err := client.GetRunnerScaleSet(ctx, groupID, c.name)
	if err != nil {
		return fmt.Errorf("lookup scale set: %w", err)
	}
	if set == nil {
		set, err = client.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{
			Name:          c.name,
			RunnerGroupID: groupID,
			Labels:        buildLabels(c.name, c.labels),
			RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
		})
		if err != nil {
			return fmt.Errorf("create scale set: %w", err)
		}
		log.Info("scale set created", "id", set.ID)
	} else {
		log.Info("resuming existing scale set", "id", set.ID)
		// Keep labels in sync with the flags so changing --labels takes effect
		// without recreating the scale set (and losing queued jobs).
		want := buildLabels(c.name, c.labels)
		if !sameLabels(set.Labels, want) {
			if _, err := client.UpdateRunnerScaleSet(ctx, set.ID, &scaleset.RunnerScaleSet{
				Name: set.Name, RunnerGroupID: set.RunnerGroupID, Labels: want,
				RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
			}); err != nil {
				log.Warn("could not update labels", "err", err)
			} else {
				log.Info("labels updated", "labels", labelNames(want))
			}
		}
	}
	client.SetSystemInfo(sysInfo(set.ID))
	if c.deleteOnExit {
		defer func() {
			if err := client.DeleteRunnerScaleSet(context.WithoutCancel(ctx), set.ID); err != nil {
				log.Warn("delete scale set", "err", err)
			}
		}()
	}

	host, _ := os.Hostname()
	scope := c.psiCgroup
	if scope == "" {
		scope = "host"
	}
	hub := telemetry.New(telemetry.Config{
		Version: version, Backend: c.backendName, Host: host, ScaleSet: c.name, URL: c.url,
		Cores: runtime.NumCPU(), MemTotalMB: first.MemTotalMB, Min: c.min, Max: maxRunners,
		PSIScope: scope, IdleTTLSec: int(c.idleTTL.Seconds()), MaxRunMin: int(c.runtimeMax.Minutes()),
	})
	prodRel := strings.Trim(strings.TrimPrefix(c.psiCgroup, "/sys/fs/cgroup"), "/")
	if prodRel == "" {
		prodRel = "system.slice"
	}
	runlightRel := strings.Trim(c.cgroup, "/")
	if runlightRel == "" {
		runlightRel = "runlight.slice"
	}
	maxMem := first.MemTotalMB / 2
	if maxMem == 0 {
		maxMem = 8192
	}
	col := newCollector(hub, prodRel, runlightRel, uint64(c.memPerRun), maxMem)
	hub.Eventf("info", "Runlight %s arrancó: máximo %d runners, reserva de memoria %d MB, presión medida en %s.", version, maxRunners, c.memReserve, scope)
	session, err := openSession(ctx, client, set.ID, host, log)
	if err != nil {
		return fmt.Errorf("session: %w", err)
	}
	defer session.Close(context.WithoutCancel(ctx))

	cpus := c.cpusPerRun
	if cpus <= 0 {
		// Share the machine fairly: cores / max concurrent runners, min 1.
		cpus = max(1, float64(runtime.NumCPU())/float64(maxRunners))
	}
	be, err := newBackend(c, log)
	if err != nil {
		return err
	}
	if sw, ok := be.(interface{ Sweep(context.Context) error }); ok {
		if err := sw.Sweep(ctx); err != nil {
			log.Warn("could not sweep stale runners", "err", err)
		}
	}

	sc := scaler.New(client, session, be, gov, scaler.Options{
		ScaleSetID: set.ID,
		MinRunners: c.min,
		IdleTTL:    c.idleTTL,
		MemoryMB:   c.memPerRun * 3 / 2, // hard cap above the soft budget
		CPUs:       cpus,
		Logger:     log,
		Tel:        hub,
		UnitStats:  col.UnitStats,
		Slots:      maxRunners,
	})

	var jan *janitor.Janitor
	if len(c.caches)+len(c.ephemerals) > 0 {
		jan = &janitor.Janitor{
			Interval: c.janitorEvery, DryRun: c.janitorDry, Tel: hub, Log: log,
			Live: func() ([]string, int) { _, busy := sc.Count(); return sc.Names(), busy },
			Keep: scaler.IsSlotName,
			Disk: func() (uint64, uint64) {
				s, err := mon.Sample()
				if err != nil {
					return 0, 0
				}
				return s.DiskFreeMB, s.DiskTotalMB
			},
		}
		for _, spec := range c.caches {
			r, err := janitor.ParseRule(spec)
			if err != nil {
				return fmt.Errorf("--cache %q: %w", spec, err)
			}
			jan.Rules = append(jan.Rules, r)
		}
		for _, spec := range c.ephemerals {
			e, err := janitor.ParseEphemeral(spec)
			if err != nil {
				return fmt.Errorf("--cache-ephemeral %q: %w", spec, err)
			}
			jan.Ephemerals = append(jan.Ephemerals, e)
		}
		go jan.Run(ctx)
		log.Info("cache janitor enabled", "rules", len(jan.Rules), "ephemeral", len(jan.Ephemerals), "dry_run", c.janitorDry)
	}
	defer sc.Shutdown(context.WithoutCancel(ctx))

	l, err := listener.New(session, listener.Config{ScaleSetID: set.ID, MaxRunners: gov.Limit(), Logger: log})
	if err != nil {
		return fmt.Errorf("listener: %w", err)
	}

	// Persistent state: push subscriptions, VAPID keys, edited settings and the
	// unclean-shutdown marker all live in --state-dir.
	stateDir, why := prepareStateDir(c.stateDir)
	var (
		ps         *push.Store
		mgr        *settings.Manager
		unexpected bool
	)
	if stateDir == "" {
		log.Warn("state dir unavailable: push notifications and editable settings are disabled", "reason", why)
	} else {
		if ps, err = push.Open(stateDir, c.vapidSubject); err != nil {
			log.Warn("push disabled", "err", err)
			ps = nil
		}
		var clearMarker func()
		var merr error
		unexpected, clearMarker, merr = markRunning(stateDir)
		if merr != nil {
			log.Warn("could not write the running marker; restart detection is off", "err", merr)
		}
		// Only a clean shutdown (signal) removes the marker: a crash or kill
		// leaves it behind and the next start raises the restart alert.
		defer func() {
			if ctx.Err() != nil {
				clearMarker()
			}
		}()
		if unexpected {
			log.Warn("previous run did not shut down cleanly")
		}
	}

	var baseRules []janitor.Rule
	if jan != nil {
		baseRules = append(baseRules, jan.Rules...)
	}
	lv := &live{gov: gov, sc: sc, jan: jan, hub: hub, col: col, log: log, baseRules: baseRules, tcfg: hub.Snapshot().Config}
	lv.memReserveMB.Store(uint64(c.memReserve))
	lv.cpuHighBits.Store(math.Float64bits(gp.CPUHigh))
	sink := newAlertSink(ctx, ps, hub, log)
	eng := alerts.New(alerts.DefaultConfig(), sink)
	lv.eng = eng

	if stateDir != "" {
		fields := settings.StaticFields(settingDefaults(c, maxRunners, gp))
		fields = append(fields, settings.CacheFields(cacheCaps(baseRules))...)
		fields = append(fields, settings.AlertFields(alerts.RuleCatalog())...)
		mgr, err = settings.Open(filepath.Join(stateDir, "settings.json"), fields, settings.WithValidator(validateSettings))
		if err != nil {
			log.Warn("editable settings disabled", "err", err)
			mgr = nil
		} else {
			lv.latest = mgr.Values
			mgr.OnChange(func(changed []string, values map[string]any) {
				lv.apply(values)
				log.Info("settings changed", "keys", changed)
				hub.Eventf("info", "Configuración cambiada desde el panel: %s.", strings.Join(changed, ", "))
			})
			lv.apply(mgr.Values()) // saved overrides take effect at boot
		}
	}

	restart := unexpected
	evaluate := func(now time.Time, sig governor.Signals, total, busy, limit int) {
		hub.SetPaused(sc.Paused())
		eng.Evaluate(ctx, alertInputs(now, sig, hub, gov, lv, total, busy, limit, restart))
		restart = false // only the first evaluation reports the restart
	}

	go sc.WatchExits(ctx)
	go control(ctx, c.tick, mon, gov, sc, l, col, log, evaluate)

	if c.listen != "" {
		dashOpts := []dashboard.Option{dashboard.WithAlerts(eng), dashboard.WithActions(&actions{sc: sc, jan: jan, hub: hub})}
		if ps != nil {
			dashOpts = append(dashOpts, dashboard.WithPush(ps))
		}
		if mgr != nil {
			dashOpts = append(dashOpts, dashboard.WithSettings(mgr))
		}
		if c.dashUser != "" {
			key, _ := hex.DecodeString(os.Getenv("RUNLIGHT_SESSION_KEY"))
			auth, err := dashboard.NewAuth(c.dashUser, os.Getenv("RUNLIGHT_DASH_PASSWORD_HASH"), key)
			if err != nil {
				return fmt.Errorf("dashboard login: %w", err)
			}
			dashOpts = append(dashOpts, dashboard.WithAuth(auth))
			log.Info("dashboard login enabled", "user", c.dashUser)
		} else {
			log.Warn("dashboard has no login: keep it behind an authenticating proxy")
		}
		srv := &http.Server{Addr: c.listen, Handler: dashboard.Handler(hub, dashOpts...), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("dashboard stopped", "err", err)
			}
		}()
		defer func() {
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(sctx)
		}()
		log.Info("dashboard listening", "addr", c.listen)
	}

	log.Info("ready", "runs-on", c.name, "scale_set_id", set.ID)
	return l.Run(ctx, sc)
}

// control is the feedback loop: sample the host, let the governor decide,
// and push the result to the listener so GitHub only hands us work we can run.
func control(ctx context.Context, every time.Duration, mon sysmon.Sampler, gov *governor.Governor,
	sc *scaler.Scaler, l *listener.Listener, col *collector, log *slog.Logger,
	afterTick func(now time.Time, sig governor.Signals, total, busy, limit int)) {
	t := time.NewTicker(every)
	defer t.Stop()
	last := -1
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			sig, err := mon.Sample()
			if err != nil {
				log.Warn("sample", "err", err)
				continue
			}
			total, busy := sc.Count()
			limit := gov.Update(now, sig, total)
			l.SetMaxRunners(limit)
			sc.Reap(ctx, now)
			col.record(now, sig, gov, sc, total, busy, limit)
			if afterTick != nil {
				afterTick(now, sig, total, busy, limit)
			}
			if limit != last {
				log.Info("capacity changed", "limit", limit, "reason", gov.Reason(),
					"runners", total, "busy", busy,
					"cpu_psi", sig.CPUSome, "mem_psi", sig.MemSome, "io_psi", sig.IOSome,
					"mem_avail_mb", sig.MemAvailMB)
				last = limit
			}
		}
	}
}

// autoMax picks a ceiling from the hardware: one runner per core, but never
// more than memory can hold at the per-runner budget.
func autoMax(s governor.Signals, memPerRunMB int) int {
	byCPU := runtime.NumCPU()
	byMem := byCPU
	if s.MemAvailMB > 0 && memPerRunMB > 0 {
		byMem = int(s.MemAvailMB) / memPerRunMB
	}
	return max(1, min(byCPU, byMem))
}

func buildLabels(name, extra string) []scaleset.Label {
	labels := []scaleset.Label{{Name: name}}
	for _, l := range strings.Split(extra, ",") {
		if l = strings.TrimSpace(l); l != "" && l != name {
			labels = append(labels, scaleset.Label{Name: l})
		}
	}
	return labels
}

func sysInfo(id int) scaleset.SystemInfo {
	return scaleset.SystemInfo{System: "runlight", Subsystem: "runlight", CommitSHA: "NA", Version: version, ScaleSetID: id}
}

func newLogger(c config) *slog.Logger {
	lvl := slog.LevelInfo
	switch strings.ToLower(c.logLevel) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if c.logJSON {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dannyjp49/runlight/internal/alerts"
	"github.com/dannyjp49/runlight/internal/governor"
	"github.com/dannyjp49/runlight/internal/janitor"
	"github.com/dannyjp49/runlight/internal/settings"
	"github.com/dannyjp49/runlight/internal/telemetry"
)

func TestRunningMarkerDetectsUncleanShutdown(t *testing.T) {
	dir := t.TempDir()
	if unexpected, clear, err := markRunning(dir); err != nil || unexpected {
		t.Fatalf("first start: unexpected=%v err=%v", unexpected, err)
	} else {
		_ = clear
	}
	// No clear() was called: a crash. The next start must notice.
	unexpected, clear, err := markRunning(dir)
	if err != nil || !unexpected {
		t.Fatalf("after crash: unexpected=%v err=%v", unexpected, err)
	}
	clear() // clean shutdown
	if unexpected, _, _ := markRunning(dir); unexpected {
		t.Fatal("clean shutdown still flagged as unexpected")
	}
}

func TestPrepareStateDir(t *testing.T) {
	if d, why := prepareStateDir(""); d != "" || why == "" {
		t.Fatalf("empty dir = %q %q", d, why)
	}
	p := filepath.Join(t.TempDir(), "a", "b")
	if d, why := prepareStateDir(p); d != p || why != "" {
		t.Fatalf("dir = %q %q", d, why)
	}
}

func TestValidateSettings(t *testing.T) {
	if e := validateSettings(map[string]any{"scaling.minRunners": 2, "scaling.maxRunners": 4, "scaling.memReserveMb": 1024}); e != nil {
		t.Fatalf("valid rejected: %v", e)
	}
	e := validateSettings(map[string]any{"scaling.minRunners": 5, "scaling.maxRunners": 4, "scaling.memReserveMb": 100})
	if e["scaling.minRunners"] == "" || e["scaling.memReserveMb"] == "" {
		t.Fatalf("errors = %v", e)
	}
}

func TestSchemaOpensEvenWithOutOfRangeFlags(t *testing.T) {
	c := config{min: 20, memPerRun: 1, memReserve: 10, diskMin: 1, idleTTL: time.Second, janitorEvery: time.Second}
	gp := governor.Defaults()
	gp.CPUHigh = 0
	caps := cacheCaps([]janitor.Rule{{Name: "npm", MaxBytes: 500 << 30}, {Name: "go", MaxBytes: 1 << 20}})
	fields := settings.StaticFields(settingDefaults(c, 64, gp))
	fields = append(fields, settings.CacheFields(caps)...)
	fields = append(fields, settings.AlertFields(alerts.RuleCatalog())...)
	mgr, err := settings.Open(filepath.Join(t.TempDir(), "s.json"), fields, settings.WithValidator(validateSettings))
	if err != nil {
		t.Fatal(err)
	}
	v := mgr.Values()
	if v["scaling.maxRunners"] != 16 || v["cache.npm.capGb"] != 200.0 || v["cache.go.capGb"] != 0.5 {
		t.Fatalf("values = %v", v)
	}
	if _, ok := v["alert.daemon_restart.enabled"]; !ok {
		t.Fatal("alert enable switch missing")
	}
	// The cross-field rule is enforced through Update.
	if _, err := mgr.Update("t", map[string]any{"scaling.maxRunners": 2, "scaling.minRunners": 5}); err == nil {
		t.Fatal("min > max accepted")
	}
}

func TestAlertConfigFromValues(t *testing.T) {
	cfg := alertConfigFrom(map[string]any{"alert.disk_low.enabled": false, "alert.disk_low.threshold": 20.0, "alert.mem_low.threshold": 2})
	if cfg[alerts.DiskLow].Enabled || cfg[alerts.DiskLow].Threshold != 20 || cfg[alerts.MemLow].Threshold != 2 || !cfg[alerts.SwapFull].Enabled {
		t.Fatalf("cfg = %+v", cfg)
	}
	if len(cfg) != len(alerts.DefaultConfig()) {
		t.Fatal("rules lost")
	}
}

func TestAlertSinkLogsEventAndSurvivesWithoutPush(t *testing.T) {
	hub := telemetry.New(telemetry.Config{})
	sink := newAlertSink(context.Background(), nil, hub, discardLogger())
	sink(context.Background(), alerts.Notification{Title: "Disco casi lleno", Body: "Quedan 7 GB", Severity: alerts.Critical})
	ev := hub.Snapshot().Events
	if len(ev) != 1 || ev[0].Kind != "alert" || !strings.Contains(ev[0].Msg, "Disco casi lleno") {
		t.Fatalf("events = %+v", ev)
	}
}

func TestAlertInputsFromTelemetry(t *testing.T) {
	hub := telemetry.New(telemetry.Config{})
	hub.JobDone(1, "r", "a/b", "j", "failed", 0, 0, 0)
	hub.JobQueued(2, "a/b", "j", nil, time.Now().Add(-5*time.Minute))
	gov := governor.New(governor.Params{Max: 2})
	lv := &live{}
	lv.memReserveMB.Store(1024)
	lv.cpuHighBits.Store(0x404E000000000000) // 60.0
	in := alertInputs(time.Now(), governor.Signals{CPUSome: 70, DiskFreeMB: 5}, hub, gov, lv, 1, 1, 2, true)
	if !in.UnexpectedRestart || in.MemReserveMB != 1024 || in.ProdCPULimit != 60 || in.ProdCPUPressure != 70 ||
		len(in.FailedJobs) != 1 || in.QueueDepth != 1 || in.QueueOldestWaitSec < 290 || in.GovernorReason == "" {
		t.Fatalf("inputs = %+v", in)
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

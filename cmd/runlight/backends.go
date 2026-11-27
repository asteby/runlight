package main

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/actions/scaleset"

	"github.com/dannyjp49/runlight/internal/backend"
)

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func newBackend(c config, log *slog.Logger) (backend.Backend, error) {
	switch c.backendName {
	case "docker":
		d := backend.NewDocker(c.image, log)
		d.CgroupParent = c.cgroup
		return d, nil
	case "systemd":
		if c.templateDir == "" || c.jobsDir == "" {
			return nil, errors.New("--backend systemd needs --template-dir and --jobs-dir")
		}
		s := backend.NewSystemd(c.templateDir, c.jobsDir, c.runAs, log)
		s.Slice = c.cgroup
		s.Nice = c.nice
		s.RuntimeMax = c.runtimeMax
		s.Env = c.envVars
		return s, nil
	}
	return nil, fmt.Errorf("unknown --backend %q", c.backendName)
}

func labelNames(ls []scaleset.Label) []string {
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		out = append(out, l.Name)
	}
	return out
}

// sameLabels compares label sets case-insensitively and order-independently.
func sameLabels(a, b []scaleset.Label) bool {
	norm := func(ls []scaleset.Label) []string {
		n := labelNames(ls)
		for i := range n {
			n[i] = strings.ToLower(n[i])
		}
		slices.Sort(n)
		return n
	}
	return slices.Equal(norm(a), norm(b))
}

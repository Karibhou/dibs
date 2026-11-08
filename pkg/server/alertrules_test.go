package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/hivecommons/dibs/pkg/match"
	"github.com/hivecommons/dibs/pkg/store"
)

// The PrometheusRule in deploy/monitoring/ is only schema-checked by CI
// (kubeconform); nothing ties the metric names, label names and label
// values hard-coded in its expressions to what /metrics really emits. A
// rename on either side silently turns an alert into one that can never
// fire. These tests pin the two sides together.

const (
	alertRulesPath = "../../deploy/monitoring/prometheusrule.yaml"
	runbookPath    = "../../runbooks/incident-response.md"
	runbookURLBase = "https://github.com/hivecommons/dibs/blob/main/runbooks/incident-response.md#"
)

type alertRule struct {
	Alert       string            `yaml:"alert"`
	Expr        string            `yaml:"expr"`
	For         string            `yaml:"for"`
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations"`
}

type prometheusRule struct {
	Kind string `yaml:"kind"`
	Spec struct {
		Groups []struct {
			Name  string      `yaml:"name"`
			Rules []alertRule `yaml:"rules"`
		} `yaml:"groups"`
	} `yaml:"spec"`
}

func loadAlertRules(t *testing.T) []alertRule {
	t.Helper()
	raw, err := os.ReadFile(alertRulesPath)
	if err != nil {
		t.Fatalf("read %s: %v", alertRulesPath, err)
	}
	var pr prometheusRule
	if err := yaml.Unmarshal(raw, &pr); err != nil {
		t.Fatalf("parse %s: %v", alertRulesPath, err)
	}
	if pr.Kind != "PrometheusRule" {
		t.Fatalf("kind = %q, want PrometheusRule", pr.Kind)
	}
	var rules []alertRule
	for _, g := range pr.Spec.Groups {
		rules = append(rules, g.Rules...)
	}
	if len(rules) == 0 {
		t.Fatal("no alert rules found")
	}
	return rules
}

// exposition is what /metrics emitted: metric name → label name → set of
// observed values.
type exposition map[string]map[string]map[string]bool

var sampleLine = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(?:\{([^}]*)\})? `)
var labelPair = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"`)

func parseExposition(t *testing.T, body string) exposition {
	t.Helper()
	ex := exposition{}
	for _, line := range strings.Split(body, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := sampleLine.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("unparsable exposition line %q", line)
		}
		name := m[1]
		if ex[name] == nil {
			ex[name] = map[string]map[string]bool{}
		}
		for _, lp := range labelPair.FindAllStringSubmatch(m[2], -1) {
			if ex[name][lp[1]] == nil {
				ex[name][lp[1]] = map[string]bool{}
			}
			ex[name][lp[1]][lp[2]] = true
		}
	}
	return ex
}

// scrapeAllFamilies drives one sample through every metric family the
// alerts select on — a failing /readyz request, a failing background job
// and a failing match-engine LLM call — then scrapes /metrics.
func scrapeAllFamilies(t *testing.T) exposition {
	t.Helper()
	reqStats.record("GET", routeGroup("", "/readyz"), http.StatusInternalServerError, time.Millisecond)
	reqStats.record("GET", routeGroup("", "/api/ideas"), http.StatusOK, time.Millisecond)
	// Every job needs one success so the last-success gauge carries every
	// job value the stale alert selects on; the failure feeds result="error".
	RecordJob(JobCatalogRefresh, nil)
	RecordJob(JobCatalogRefresh, errors.New("boom"))
	RecordJob(JobRegistrySync, nil)

	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream down", http.StatusBadGateway)
	}))
	t.Cleanup(gateway.Close)
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	idea := &store.Idea{Title: "Alert contract", Body: "First paragraph.\n\nSecond.", Author: "alice", Visibility: store.VisibilityPublic}
	if err := st.Create(idea); err != nil {
		t.Fatalf("store.Create: %v", err)
	}
	eng := &match.Engine{Store: st, LLM: &match.LLM{BaseURL: gateway.URL + "/v1", Model: "test"}}
	if _, err := eng.EnsureTLDR(context.Background(), idea); err != nil {
		t.Fatalf("EnsureTLDR: %v", err)
	}

	rr := httptest.NewRecorder()
	MetricsHandler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d", rr.Code)
	}
	return parseExposition(t, rr.Body.String())
}

var (
	// dibs_http_requests_total{status_class="5xx",route_group!="readyz"}
	selectorRe = regexp.MustCompile(`\b(dibs_[a-z0-9_]+)(?:\{([^}]*)\})?`)
	matcherRe  = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)\s*(=~|!~|!=|=)\s*"((?:[^"\\]|\\.)*)"`)
	byClauseRe = regexp.MustCompile(`\bby\s*\(([^)]*)\)`)
	tmplLabel  = regexp.MustCompile(`\{\{\s*\$labels\.([a-zA-Z_][a-zA-Z0-9_]*)\s*\}\}`)
)

func splitLabelList(s string) []string {
	var out []string
	for _, l := range strings.Split(s, ",") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func TestAlertRulesSelectEmittedMetrics(t *testing.T) {
	ex := scrapeAllFamilies(t)
	rules := loadAlertRules(t)

	selectsDibs := 0
	for _, r := range rules {
		sels := selectorRe.FindAllStringSubmatch(r.Expr, -1)
		if len(sels) == 0 {
			// e.g. DibsDown selects on the scraper's own `up` series.
			continue
		}
		selectsDibs++
		for _, sel := range sels {
			name, matchers := sel[1], sel[2]
			labels, ok := ex[name]
			if !ok {
				t.Errorf("%s: selects %s, which /metrics never emits", r.Alert, name)
				continue
			}
			for _, m := range matcherRe.FindAllStringSubmatch(matchers, -1) {
				label, op, value := m[1], m[2], m[3]
				values, ok := labels[label]
				if !ok {
					t.Errorf("%s: %s has no label %q (emitted: %v)", r.Alert, name, label, keys(labels))
					continue
				}
				if (op == "=" || op == "!=") && label != "le" && !values[value] {
					// A matcher on a value the exporter never produces is a
					// silent no-op (=) or an ineffective exclusion (!=).
					t.Errorf("%s: %s{%s%s%q}: value never emitted (seen: %v)", r.Alert, name, label, op, value, keys(values))
				}
			}
			for _, bc := range byClauseRe.FindAllStringSubmatch(r.Expr, -1) {
				for _, by := range splitLabelList(bc[1]) {
					if _, ok := labels[by]; !ok {
						t.Errorf("%s: aggregates by %q but %s has no such label", r.Alert, by, name)
					}
				}
			}
		}
	}
	if selectsDibs == 0 {
		t.Fatal("no alert selects a dibs_* metric; test is not exercising anything")
	}
}

func TestAlertRuleTemplatesReferenceSurvivingLabels(t *testing.T) {
	ex := scrapeAllFamilies(t)
	for _, r := range loadAlertRules(t) {
		var byLabels map[string]bool
		for _, bc := range byClauseRe.FindAllStringSubmatch(r.Expr, -1) {
			if byLabels == nil {
				byLabels = map[string]bool{}
			}
			for _, l := range splitLabelList(bc[1]) {
				byLabels[l] = true
			}
		}
		for _, ann := range []string{"summary", "description"} {
			for _, m := range tmplLabel.FindAllStringSubmatch(r.Annotations[ann], -1) {
				label := m[1]
				if byLabels != nil {
					if !byLabels[label] {
						t.Errorf("%s: %s uses {{ $labels.%s }} but the expression aggregates by %v, dropping it", r.Alert, ann, label, keys(byLabels))
					}
					continue
				}
				// No aggregation: the label must survive from at least one
				// selected metric.
				found := false
				for _, sel := range selectorRe.FindAllStringSubmatch(r.Expr, -1) {
					if _, ok := ex[sel[1]][label]; ok {
						found = true
					}
				}
				if !found {
					t.Errorf("%s: %s uses {{ $labels.%s }} but no selected metric emits that label", r.Alert, ann, label)
				}
			}
		}
	}
}

func TestAlertRulesWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range loadAlertRules(t) {
		if r.Alert == "" {
			t.Fatalf("rule with empty alert name: %+v", r)
		}
		if seen[r.Alert] {
			t.Errorf("duplicate alert name %s", r.Alert)
		}
		seen[r.Alert] = true
		if strings.TrimSpace(r.Expr) == "" {
			t.Errorf("%s: empty expr", r.Alert)
		}
		if r.For == "" {
			t.Errorf("%s: no `for:` — a single bad scrape would page", r.Alert)
		}
		switch r.Labels["severity"] {
		case "warning", "critical":
		default:
			t.Errorf("%s: severity = %q, want warning|critical", r.Alert, r.Labels["severity"])
		}
		if strings.TrimSpace(r.Annotations["summary"]) == "" {
			t.Errorf("%s: missing summary annotation", r.Alert)
		}
		if _, ok := r.Annotations["runbook_url"]; !ok {
			t.Errorf("%s: missing runbook_url annotation", r.Alert)
		}
	}
}

// githubAnchor mirrors GitHub's markdown heading → fragment slug.
func githubAnchor(heading string) string {
	s := strings.ToLower(strings.TrimSpace(heading))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('-')
		}
	}
	return b.String()
}

func TestAlertRunbookAnchorsExist(t *testing.T) {
	raw, err := os.ReadFile(runbookPath)
	if err != nil {
		t.Fatalf("read %s: %v", runbookPath, err)
	}
	anchors := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "#") {
			anchors[githubAnchor(strings.TrimLeft(line, "# "))] = true
		}
	}
	for _, r := range loadAlertRules(t) {
		u := r.Annotations["runbook_url"]
		if !strings.HasPrefix(u, runbookURLBase) {
			t.Errorf("%s: runbook_url %q does not point at %s", r.Alert, u, filepath.Base(runbookPath))
			continue
		}
		if frag := strings.TrimPrefix(u, runbookURLBase); !anchors[frag] {
			t.Errorf("%s: runbook anchor #%s not found in %s (have %v)", r.Alert, frag, runbookPath, keys(anchors))
		}
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

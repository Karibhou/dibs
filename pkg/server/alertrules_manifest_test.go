package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestAlertRulesSelectEmittedMetrics skips every selector that is not a
// dibs_* metric, because /metrics never emits those series: DibsDown reads
// the scraper's own `up`, DibsPodRestarting reads kube-state-metrics. Their
// label matchers name Kubernetes objects from deploy/ instead — the
// namespace, the container, and the Service (which the Prometheus Operator
// turns into the `job` label). Renaming any of those in deploy/ leaves the
// alert selecting an empty vector, which kubeconform cannot see and which
// never fires. These tests pin those matchers to the manifests.

const deployDir = "../../deploy"

// kubeStateMetrics lists the kube-state-metrics series the alert rules are
// allowed to select on. The exporter is not part of this repository, so a
// typo in a kube_* name cannot be caught against a live exposition; it is
// caught here instead, by having to be added deliberately.
var kubeStateMetrics = map[string]bool{
	"kube_pod_container_status_restarts_total": true,
}

type k8sMetadata struct {
	Name      string            `yaml:"name"`
	Namespace string            `yaml:"namespace"`
	Labels    map[string]string `yaml:"labels"`
}

type k8sNamespace struct {
	Kind     string      `yaml:"kind"`
	Metadata k8sMetadata `yaml:"metadata"`
}

type k8sDeployment struct {
	Kind     string      `yaml:"kind"`
	Metadata k8sMetadata `yaml:"metadata"`
	Spec     struct {
		Template struct {
			Metadata k8sMetadata `yaml:"metadata"`
			Spec     struct {
				Containers []struct {
					Name  string `yaml:"name"`
					Ports []struct {
						Name string `yaml:"name"`
					} `yaml:"ports"`
				} `yaml:"containers"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

type k8sService struct {
	Kind     string      `yaml:"kind"`
	Metadata k8sMetadata `yaml:"metadata"`
	Spec     struct {
		Selector map[string]string `yaml:"selector"`
		Ports    []struct {
			Name       string `yaml:"name"`
			TargetPort string `yaml:"targetPort"`
		} `yaml:"ports"`
	} `yaml:"spec"`
}

type k8sServiceMonitor struct {
	Kind     string      `yaml:"kind"`
	Metadata k8sMetadata `yaml:"metadata"`
	Spec     struct {
		JobLabel string `yaml:"jobLabel"`
		Selector struct {
			MatchLabels map[string]string `yaml:"matchLabels"`
		} `yaml:"selector"`
		Endpoints []struct {
			Port        string `yaml:"port"`
			Path        string `yaml:"path"`
			Interval    string `yaml:"interval"`
			HonorLabels bool   `yaml:"honorLabels"`
		} `yaml:"endpoints"`
	} `yaml:"spec"`
}

func loadManifest(t *testing.T, name, wantKind string, into interface{ kind() string }) {
	t.Helper()
	path := filepath.Join(deployDir, name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := yaml.Unmarshal(raw, into); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if got := into.kind(); got != wantKind {
		t.Fatalf("%s: kind = %q, want %s", path, got, wantKind)
	}
}

func (n *k8sNamespace) kind() string      { return n.Kind }
func (d *k8sDeployment) kind() string     { return d.Kind }
func (s *k8sService) kind() string        { return s.Kind }
func (m *k8sServiceMonitor) kind() string { return m.Kind }

type deployManifests struct {
	ns      k8sNamespace
	deploy  k8sDeployment
	svc     k8sService
	monitor k8sServiceMonitor
}

func loadDeployManifests(t *testing.T) deployManifests {
	t.Helper()
	var m deployManifests
	loadManifest(t, "namespace.yaml", "Namespace", &m.ns)
	loadManifest(t, "deployment.yaml", "Deployment", &m.deploy)
	loadManifest(t, "service.yaml", "Service", &m.svc)
	loadManifest(t, filepath.Join("monitoring", "servicemonitor.yaml"), "ServiceMonitor", &m.monitor)
	return m
}

// scrapeJobLabel is the value Prometheus Operator assigns to `job` for
// targets found through a ServiceMonitor: the Service's label named by
// spec.jobLabel if set, otherwise the Service name.
func scrapeJobLabel(m deployManifests) string {
	if m.monitor.Spec.JobLabel != "" {
		return m.svc.Metadata.Labels[m.monitor.Spec.JobLabel]
	}
	return m.svc.Metadata.Name
}

func TestAlertRulesNonAppSelectorsMatchDeployManifests(t *testing.T) {
	m := loadDeployManifests(t)
	ns := m.ns.Metadata.Name
	job := scrapeJobLabel(m)
	containers := map[string]bool{}
	for _, c := range m.deploy.Spec.Template.Spec.Containers {
		containers[c.Name] = true
	}

	checked := 0
	for _, r := range loadAlertRules(t) {
		for _, sel := range nonAppSelectors(r.Expr) {
			name, matchers := sel[0], sel[1]
			if strings.HasPrefix(name, "kube_") && !kubeStateMetrics[name] {
				t.Errorf("%s: selects %s, which is not a known kube-state-metrics series (add it to kubeStateMetrics deliberately)", r.Alert, name)
			}
			for _, mm := range matcherRe.FindAllStringSubmatch(matchers, -1) {
				label, op, value := mm[1], mm[2], mm[3]
				if op != "=" {
					// Negative and regex matchers do not pin a single object.
					continue
				}
				checked++
				switch label {
				case "namespace":
					if value != ns {
						t.Errorf("%s: %s{namespace=%q} but deploy/namespace.yaml is %q", r.Alert, name, value, ns)
					}
				case "container":
					if !containers[value] {
						t.Errorf("%s: %s{container=%q} but deploy/deployment.yaml containers are %v", r.Alert, name, value, keys(containers))
					}
				case "job":
					if value != job {
						t.Errorf("%s: %s{job=%q} but the ServiceMonitor scrape job is %q (Service name / jobLabel)", r.Alert, name, value, job)
					}
				default:
					t.Errorf("%s: %s{%s=%q}: label is not pinned to any deploy/ manifest; extend this test before relying on it", r.Alert, name, label, value)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no non-dibs_ selector carries an equality matcher; test is not exercising anything")
	}
}

// nonAppSelectors returns the (metric name, raw matchers) pairs in expr whose
// metric is not one of ours — i.e. the ones TestAlertRulesSelectEmittedMetrics
// cannot verify against /metrics.
func nonAppSelectors(expr string) [][2]string {
	// `sum by (job)` would otherwise surface `job` as a metric name.
	expr = byClauseRe.ReplaceAllString(expr, "")
	var out [][2]string
	for _, m := range identSelectorRe.FindAllStringSubmatch(expr, -1) {
		name := m[1]
		if strings.HasPrefix(name, "dibs_") || promFunctions[name] || promKeywords[name] {
			continue
		}
		out = append(out, [2]string{name, m[2]})
	}
	return out
}

var (
	// identSelectorRe is selectorRe without the dibs_ prefix: any PromQL
	// identifier, optionally followed by a matcher block. A consumed block
	// also swallows the label names inside it, so only metric positions
	// surface; PromQL functions and keywords are filtered by nonAppSelectors.
	identSelectorRe = regexp.MustCompile(`\b([a-zA-Z_:][a-zA-Z0-9_:]*)(?:\{([^}]*)\})?`)

	promFunctions = map[string]bool{
		"sum": true, "rate": true, "increase": true, "absent": true, "time": true,
		"histogram_quantile": true, "max": true, "min": true, "avg": true, "count": true,
	}
	promKeywords = map[string]bool{
		"by": true, "on": true, "and": true, "or": true, "unless": true, "without": true,
		"le": true, "for": true, "group_left": true, "group_right": true, "ignoring": true,
	}
)

func TestServiceMonitorScrapesDeclaredMetricsPort(t *testing.T) {
	m := loadDeployManifests(t)

	if got, want := m.monitor.Metadata.Namespace, m.ns.Metadata.Name; got != want {
		t.Errorf("servicemonitor namespace = %q, want %q", got, want)
	}
	if got, want := m.svc.Metadata.Namespace, m.ns.Metadata.Name; got != want {
		t.Errorf("service namespace = %q, want %q", got, want)
	}
	if got, want := m.deploy.Metadata.Namespace, m.ns.Metadata.Name; got != want {
		t.Errorf("deployment namespace = %q, want %q", got, want)
	}

	// ServiceMonitor → Service: the selector must match the Service labels.
	if len(m.monitor.Spec.Selector.MatchLabels) == 0 {
		t.Fatal("servicemonitor has no selector.matchLabels; it would select nothing")
	}
	for k, v := range m.monitor.Spec.Selector.MatchLabels {
		if m.svc.Metadata.Labels[k] != v {
			t.Errorf("servicemonitor selects %s=%q but the Service is labelled %v", k, v, m.svc.Metadata.Labels)
		}
	}
	// Service → Pods: the Service selector must match the pod template labels.
	for k, v := range m.svc.Spec.Selector {
		if m.deploy.Spec.Template.Metadata.Labels[k] != v {
			t.Errorf("service selects %s=%q but the pod template is labelled %v", k, v, m.deploy.Spec.Template.Metadata.Labels)
		}
	}

	// ServiceMonitor endpoint port → Service port → container port, by name.
	svcPorts := map[string]string{}
	for _, p := range m.svc.Spec.Ports {
		svcPorts[p.Name] = p.TargetPort
	}
	containerPorts := map[string]bool{}
	for _, c := range m.deploy.Spec.Template.Spec.Containers {
		for _, p := range c.Ports {
			containerPorts[p.Name] = true
		}
	}
	if len(m.monitor.Spec.Endpoints) == 0 {
		t.Fatal("servicemonitor has no endpoints")
	}
	for _, ep := range m.monitor.Spec.Endpoints {
		target, ok := svcPorts[ep.Port]
		if !ok {
			t.Errorf("servicemonitor scrapes Service port %q but the Service only has %v", ep.Port, keys(svcPorts))
			continue
		}
		if !containerPorts[target] {
			t.Errorf("Service port %q targets container port %q, which no container declares (have %v)", ep.Port, target, keys(containerPorts))
		}
		if ep.Path != "" && ep.Path != "/metrics" {
			t.Errorf("servicemonitor scrapes %q; MetricsHandler is served at /metrics", ep.Path)
		}
		if !ep.HonorLabels {
			t.Errorf("servicemonitor endpoint does not set honorLabels: true; background-job metrics will lose their job identity")
		}
	}
}

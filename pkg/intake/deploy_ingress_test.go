package intake

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// deployDir is the Kubernetes manifest set this binary ships with. The intake
// upload cap is enforced twice on that path: ingress-nginx rejects bodies over
// proxy-body-size with a 413 before dibs sees them, and HandleUpload rejects
// bodies over MaxUploadBytes()+1 MiB. The ingress limit must stay at or above
// the app limit, or every upload between the two is refused with an nginx
// 413 that the app's "intake upload is too large" handling never produces.
const deployDir = "../../deploy"

const proxyBodySizeAnnotation = "nginx.ingress.kubernetes.io/proxy-body-size"

// multipartSlack mirrors the 1024*1024 bytes HandleUpload adds to the
// MaxBytesReader limit over the file cap for the multipart framing.
const multipartSlack = 1024 * 1024

type k8sIngress struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name        string            `yaml:"name"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
}

type k8sConfigMap struct {
	Kind string            `yaml:"kind"`
	Data map[string]string `yaml:"data"`
}

var nginxSizeRe = regexp.MustCompile(`^(\d+)([kKmMgG]?)$`)

// parseNginxSize reads an nginx size directive value (bare bytes or a k/m/g
// suffix, 1024-based, as client_max_body_size accepts it).
func parseNginxSize(t *testing.T, raw string) int64 {
	t.Helper()
	m := nginxSizeRe.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		t.Fatalf("%s=%q is not an nginx size (digits with optional k/m/g suffix)", proxyBodySizeAnnotation, raw)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		t.Fatalf("%s=%q: %v", proxyBodySizeAnnotation, raw, err)
	}
	switch strings.ToLower(m[2]) {
	case "k":
		n <<= 10
	case "m":
		n <<= 20
	case "g":
		n <<= 30
	}
	return n
}

func readDeployYAML(t *testing.T, name string, out any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(deployDir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if err := yaml.Unmarshal(raw, out); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
}

func TestIngressProxyBodySizeCoversIntakeUploadCap(t *testing.T) {
	var ing k8sIngress
	readDeployYAML(t, "ingress.yaml", &ing)
	if ing.Kind != "Ingress" {
		t.Fatalf("deploy/ingress.yaml kind = %q, want Ingress", ing.Kind)
	}
	raw, ok := ing.Metadata.Annotations[proxyBodySizeAnnotation]
	if !ok {
		t.Fatalf("deploy/ingress.yaml %s has no %s annotation; ingress-nginx then defaults client_max_body_size to 1m and 413s every intake upload", ing.Metadata.Name, proxyBodySizeAnnotation)
	}
	ingressLimit := parseNginxSize(t, raw)

	// Resolve the cap the way the deployed pod does: the ConfigMap is loaded
	// with envFrom, so its DIBS_INTAKE_MAX_MB wins and an absent key leaves
	// DefaultMaxUploadMB in force.
	var cm k8sConfigMap
	readDeployYAML(t, "configmap.yaml", &cm)
	if cm.Kind != "ConfigMap" {
		t.Fatalf("deploy/configmap.yaml kind = %q, want ConfigMap", cm.Kind)
	}
	t.Setenv("DIBS_INTAKE_MAX_MB", cm.Data["DIBS_INTAKE_MAX_MB"])
	t.Setenv("IDEATE_INTAKE_MAX_MB", "")
	appLimit := MaxUploadBytes() + multipartSlack

	if ingressLimit < appLimit {
		t.Fatalf("deploy/ingress.yaml %s=%q (%d bytes) is below the intake request limit %d bytes (DIBS_INTAKE_MAX_MB=%q, default %d MB, +1 MiB multipart slack); raise the annotation together with the cap or ingress-nginx returns 413 before dibs does",
			proxyBodySizeAnnotation, raw, ingressLimit, appLimit, cm.Data["DIBS_INTAKE_MAX_MB"], DefaultMaxUploadMB)
	}
}

func TestParseNginxSize(t *testing.T) {
	for _, tt := range []struct {
		raw  string
		want int64
	}{
		{"30m", 30 << 20},
		{"30M", 30 << 20},
		{"512k", 512 << 10},
		{"1g", 1 << 30},
		{"4096", 4096},
	} {
		if got := parseNginxSize(t, tt.raw); got != tt.want {
			t.Errorf("parseNginxSize(%q) = %d, want %d", tt.raw, got, tt.want)
		}
	}
}

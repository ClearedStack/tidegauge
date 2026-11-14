package deploy_test

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestManifestsAndRepositoryRules(t *testing.T) {
	tidegaugeYAML, err := os.ReadFile("tidegauge.yaml")
	if err != nil {
		t.Fatal(err)
	}
	netpolYAML, err := os.ReadFile("networkpolicy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	koYAML, err := os.ReadFile(filepath.Join("..", ".ko.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	goMod, err := os.ReadFile(filepath.Join("..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	appYAML, err := os.ReadFile(filepath.Join("..", "app.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	// 1. Module path and stdlib-only rule.
	if !strings.Contains(string(goMod), "module github.com/ClearedStack/tidegauge\n") {
		t.Fatalf("go.mod does not declare module github.com/ClearedStack/tidegauge:\n%s", goMod)
	}
	if strings.Contains(string(goMod), "require") {
		t.Fatalf("go.mod must have zero external dependencies:\n%s", goMod)
	}

	// 2. No Dockerfile allowed; ko base image and app.yaml must be pinned by sha256 digest.
	if _, err := os.Stat(filepath.Join("..", "Dockerfile")); err == nil {
		t.Fatal("Dockerfile found; images must be built with ko, not Docker")
	}
	digestRE := regexp.MustCompile(`@sha256:[0-9a-f]{64}\b`)
	if !digestRE.Match(koYAML) {
		t.Fatalf(".ko.yaml base image is not pinned by sha256 digest:\n%s", koYAML)
	}
	appStr := string(appYAML)
	for _, needle := range []string{
		"name: tidegauge",
		"namespace: cs-workers",
		"image: tidegauge",
		"replicas: 1",
		"port: 8080",
		"memory: 30Mi",
	} {
		if !strings.Contains(appStr, needle) {
			t.Errorf("app.yaml missing %q", needle)
		}
	}
	if !regexp.MustCompile(`digest: sha256:[0-9a-f]{64}\b`).Match(appYAML) {
		t.Errorf("app.yaml digest is not sha256:<64hex>:\n%s", appYAML)
	}
	allowedAppKeys := map[string]bool{
		"name": true, "namespace": true, "image": true, "digest": true,
		"replicas": true, "port": true, "memory": true, "allowFrom": true,
	}
	for _, line := range strings.Split(appStr, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "-") {
			continue
		}
		key, _, ok := strings.Cut(trimmed, ":")
		if ok && !allowedAppKeys[strings.TrimSpace(key)] {
			t.Errorf("app.yaml contains key %q rejected by Shipkeeper", key)
		}
	}

	// 3. Deployment posture in cs-workers.
	tg := string(tidegaugeYAML)
	requiredInDeploy := []string{
		"namespace: cs-workers",
		"serviceAccountName: tidegauge",
		"automountServiceAccountToken: true",
		"runAsNonRoot: true",
		"readOnlyRootFilesystem: true",
		"allowPrivilegeEscalation: false",
		`drop: ["ALL"]`,
		"key: kubernetes.io/arch",
		"operator: In",
		`values: ["arm64"]`,
		"key: kubernetes.io/hostname",
		"operator: NotIn",
		`values: ["cm3588-plus"]`,
		"memory: 16Mi",
		"memory: 30Mi",
		"mountPath: /var/spool/cs",
	}
	for _, needle := range requiredInDeploy {
		if !strings.Contains(tg, needle) {
			t.Errorf("deploy/tidegauge.yaml missing required setting %q", needle)
		}
	}
	if !digestRE.Match(tidegaugeYAML) {
		t.Error("deploy/tidegauge.yaml container image is not pinned by @sha256: digest")
	}
	if strings.Contains(tg, "kind: Secret") || strings.Contains(tg, "secretKeyRef") {
		t.Error("deploy/tidegauge.yaml must not define or reference Secrets")
	}

	// 4. Default-deny NetworkPolicy and scoped egress in cs-workers.
	np := string(netpolYAML)
	for _, needle := range []string{
		"name: default-deny",
		"namespace: cs-workers",
		"podSelector: {}",
		"- Ingress",
		"- Egress",
		"name: tidegauge-egress",
		"app.kubernetes.io/name: muster",
		"port: 8080",
		"port: 53",
		"port: 443",
		"10.0.0.0/8",
		"100.64.0.0/10",
	} {
		if !strings.Contains(np, needle) {
			t.Errorf("deploy/networkpolicy.yaml missing %q", needle)
		}
	}
	if strings.Contains(np, "5432") || strings.Contains(np, "logbook") {
		t.Error("deploy/networkpolicy.yaml must never open access to Logbook")
	}

	// 5. No em dashes anywhere in code or documentation (excluding PR #1 XML test fixtures).
	emDash := []byte("\xe2\x80\x94")
	err = filepath.WalkDir("..", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		norm := filepath.ToSlash(path)
		if strings.HasPrefix(norm, "../tools/ecfr/testdata/") && strings.HasSuffix(norm, ".xml") {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if bytes.Contains(data, emDash) {
			t.Errorf("em dash (U+2014) found in %s", norm)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

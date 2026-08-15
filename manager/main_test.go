package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestWriteSecuredConfigOverridesUnsafeControlFields(t *testing.T) {
	dir := t.TempDir()
	a := &app{}
	path := filepath.Join(dir, "config.yaml")
	raw := []byte("external-controller: 0.0.0.0:9090\nexternal-ui: unsafe\nsecret: leaked\nmode: rule\n")
	if err := a.writeSecuredConfig(raw, "generated-secret", path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := yaml.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got["external-controller"] != "127.0.0.1:9090" {
		t.Fatalf("controller not secured: %v", got["external-controller"])
	}
	if got["external-ui"] != "ui" || got["secret"] != "generated-secret" {
		t.Fatalf("managed fields not enforced: %#v", got)
	}
}

func TestGatewayPrefixRedirectAndAdminGuard(t *testing.T) {
	a := &app{configDir: t.TempDir(), secret: filepath.Join(t.TempDir(), "secret")}
	redirect := httptest.NewRecorder()
	a.routes().ServeHTTP(redirect, httptest.NewRequest(http.MethodGet, "/app/mihomo", nil))
	if redirect.Code != http.StatusTemporaryRedirect || redirect.Header().Get("Location") != "/app/mihomo/" {
		t.Fatalf("unexpected redirect: code=%d location=%q", redirect.Code, redirect.Header().Get("Location"))
	}

	forbidden := httptest.NewRecorder()
	a.routes().ServeHTTP(forbidden, httptest.NewRequest(http.MethodPost, "/app/mihomo/api/restart", nil))
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("non-admin restart returned %d", forbidden.Code)
	}
}

func TestValidateRemoteURL(t *testing.T) {
	valid := []string{"https://example.com/config.yaml", " https://example.com/sub?id=1 "}
	for _, raw := range valid {
		if _, err := validateRemoteURL(raw); err != nil {
			t.Errorf("expected valid URL %q: %v", raw, err)
		}
	}
	invalid := []string{"http://example.com/config.yaml", "file:///etc/passwd", "https://user:pass@example.com/a", "not-a-url"}
	for _, raw := range invalid {
		if _, err := validateRemoteURL(raw); err == nil {
			t.Errorf("expected invalid URL %q", raw)
		}
	}
}

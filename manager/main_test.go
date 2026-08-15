package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

func TestApplyConfigValidatesBacksUpAndRestarts(t *testing.T) {
	dir := t.TempDir()
	checker := filepath.Join(dir, "fake-mihomo")
	if err := os.WriteFile(checker, []byte("#!/bin/sh\nif [ \"$1\" = \"-t\" ]; then exit 0; fi\nsleep 30\n"), 0755); err != nil {
		t.Fatal(err)
	}
	logFile, err := os.Create(filepath.Join(dir, "mihomo.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	a := &app{
		binary: checker, configDir: dir, config: filepath.Join(dir, "config.yaml"),
		secret: filepath.Join(dir, "secret"), logFile: logFile,
	}
	if err := os.WriteFile(a.secret, []byte("server-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	original := []byte("mode: direct\nexternal-controller: 127.0.0.1:9090\nexternal-ui: ui\nsecret: server-secret\n")
	if err := os.WriteFile(a.config, original, 0600); err != nil {
		t.Fatal(err)
	}
	changed, err := a.applyConfig(context.Background(), []byte("mode: rule\nexternal-controller: 0.0.0.0:9090\nsecret: client-secret\n"), contentHash(original))
	defer a.stopCore()
	if err != nil || !changed {
		t.Fatalf("applyConfig() changed=%v err=%v", changed, err)
	}
	backup, _ := os.ReadFile(a.config + ".bak")
	if string(backup) != string(original) {
		t.Fatalf("unexpected backup: %q", backup)
	}
	updated, _ := os.ReadFile(a.config)
	var got map[string]any
	if err := yaml.Unmarshal(updated, &got); err != nil {
		t.Fatal(err)
	}
	if got["external-controller"] != "127.0.0.1:9090" || got["secret"] != "server-secret" {
		t.Fatalf("security fields not enforced: %#v", got)
	}
}

func TestApplyConfigRejectsStaleEditorVersion(t *testing.T) {
	dir := t.TempDir()
	a := &app{configDir: dir, config: filepath.Join(dir, "config.yaml"), secret: filepath.Join(dir, "secret")}
	if err := os.WriteFile(a.config, []byte("mode: direct\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.secret, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.applyConfig(context.Background(), []byte("mode: rule\n"), "stale-hash"); !errors.Is(err, errConfigConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
}

func TestReadLogChunkTailsAndFollows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("first\n"), 0600); err != nil {
		t.Fatal(err)
	}
	chunk, offset, reset, err := readLogChunk(path, -1)
	if err != nil || chunk != "first\n" || !reset {
		t.Fatalf("initial read chunk=%q offset=%d reset=%v err=%v", chunk, offset, reset, err)
	}
	file, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	_, _ = file.WriteString("second\n")
	_ = file.Close()
	chunk, _, reset, err = readLogChunk(path, offset)
	if err != nil || chunk != "second\n" || reset {
		t.Fatalf("follow read chunk=%q reset=%v err=%v", chunk, reset, err)
	}
}

func TestEmbeddedUIContainsBackNavigationAndTools(t *testing.T) {
	for _, marker := range []string{"返回管理页", "config-editor", "api/logs/stream", "dashboard-frame"} {
		if !strings.Contains(indexHTML, marker) {
			t.Errorf("embedded UI missing %q", marker)
		}
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

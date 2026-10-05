package main

import (
	"strings"
	"testing"

	"github.com/prem0x01/costblame/internal/config"
	"github.com/prem0x01/costblame/internal/correlate"
)

func TestNewServiceMap(t *testing.T) {
	sm, err := newServiceMap(config.CorrelationConfig{
		ServiceMap:   []config.ServiceMapEntry{{Match: "acme/payments", Services: []string{"lambda"}}},
		PathPatterns: []config.PathPatternEntry{{Pattern: "billing/", Service: "RDS"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := sm.ForKeys("acme/payments"); len(got) != 1 {
		t.Errorf("rule not applied: %v", got)
	}
	if got := sm.InferFromFiles([]string{"billing/x.go"}); len(got) != 1 || got[0] != "RDS" {
		t.Errorf("path pattern not applied: %v", got)
	}

	for name, cfg := range map[string]config.CorrelationConfig{
		"empty match":   {ServiceMap: []config.ServiceMapEntry{{Services: []string{"lambda"}}}},
		"no services":   {ServiceMap: []config.ServiceMapEntry{{Match: "acme/x"}}},
		"bad path rule": {PathPatterns: []config.PathPatternEntry{{Pattern: "x"}}},
	} {
		if _, err := newServiceMap(cfg); err == nil || !strings.Contains(err.Error(), "correlation config") {
			t.Errorf("%s: err = %v, want a startup error naming the config section", name, err)
		}
	}
}

func rules(t *testing.T) *correlate.ServiceMap {
	t.Helper()
	sm, err := correlate.NewServiceMap([]correlate.ServiceMapEntry{{Match: "*", Services: []string{"lambda"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return sm
}

func TestBuildDeploySources_WarnsWhenASourceCannotMatchAService(t *testing.T) {
	cfg := config.SourcesConfig{
		GitHub: config.GitHubSourceConfig{WebhookSecret: "s"},
		GitLab: config.GitLabSourceConfig{WebhookSecret: "s"},
		ArgoCD: config.ArgoCDSourceConfig{Enabled: true, Token: "t"},
	}

	// No tokens and no rules: every source is blind to services.
	sources := buildDeploySources(cfg, nil)
	warnings := serviceSignalWarnings(cfg, nil, sources)
	if len(warnings) != 3 {
		t.Fatalf("warnings = %v, want one per source", warnings)
	}
	for i, want := range []string{"github", "gitlab", "argocd"} {
		if !strings.HasPrefix(warnings[i], want+":") {
			t.Errorf("warning %d = %q, want it to start with %q", i, warnings[i], want)
		}
	}

	// A service-map rule fixes all three.
	if w := serviceSignalWarnings(cfg, rules(t), sources); len(w) != 0 {
		t.Errorf("a repo rule should silence the warnings, got %v", w)
	}

	// Tokens fix GitHub and GitLab, but never ArgoCD, which has no file list.
	cfg.GitHub.Token, cfg.GitLab.Token = "gh", "gl"
	w := serviceSignalWarnings(cfg, nil, buildDeploySources(cfg, nil))
	if len(w) != 1 || !strings.HasPrefix(w[0], "argocd:") {
		t.Errorf("warnings with tokens = %v, want only ArgoCD's", w)
	}
}

func TestServiceSignalWarnings_OnlyForEnabledSources(t *testing.T) {
	cfg := config.SourcesConfig{GitHub: config.GitHubSourceConfig{WebhookSecret: "s"}}
	sources := buildDeploySources(cfg, nil) // only GitHub is enabled
	w := serviceSignalWarnings(cfg, nil, sources)
	if len(w) != 1 || !strings.HasPrefix(w[0], "github:") {
		t.Errorf("warnings = %v, want only GitHub's", w)
	}
}

func TestResolveAPIToken(t *testing.T) {
	if tok, generated, err := resolveAPIToken("fixed"); err != nil || generated || tok != "fixed" {
		t.Errorf("configured token: %q %v %v", tok, generated, err)
	}
	a, gen, err := resolveAPIToken("")
	b, _, _ := resolveAPIToken("")
	if err != nil || !gen || len(a) < 20 || a == b {
		t.Errorf("a generated token must be long, random and flagged: %q %q gen=%v err=%v", a, b, gen, err)
	}
}

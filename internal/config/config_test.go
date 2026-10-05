package config

import (
	"os"
	"path/filepath"
	"testing"
)

// chdir switches the working directory for one test (testing.T.Chdir needs Go 1.24;
// the module targets 1.22).
func chdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
}

// In a release archive the binary "costblame" sits next to costblame.example.yaml.
// Config discovery must not try to parse the binary as YAML.
func TestLoad_IgnoresBinaryNamedCostblameInWorkingDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "costblame"), []byte("\x7fELF\x00\x01\x02binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	chdir(t, dir)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load with a binary named costblame in cwd: %v", err)
	}
	if cfg.Server.Port != 8080 {
		t.Errorf("port = %d, want default 8080", cfg.Server.Port)
	}
}

func TestLoad_ReadsCostblameYAMLFromWorkingDir(t *testing.T) {
	dir := t.TempDir()
	yaml := "server:\n  port: 9999\n  api_token: from-file\nsources:\n  argocd:\n    enabled: true\n    token: argo-file\n"
	if err := os.WriteFile(filepath.Join(dir, "costblame.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	chdir(t, dir)

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Port != 9999 || cfg.Server.APIToken != "from-file" {
		t.Errorf("server = %+v, want port 9999 / token from-file", cfg.Server)
	}
	if !cfg.Sources.ArgoCD.Enabled || cfg.Sources.ArgoCD.Token != "argo-file" {
		t.Errorf("argocd = %+v, want enabled with token argo-file", cfg.Sources.ArgoCD)
	}
}

func TestLoad_TokensFromEnv(t *testing.T) {
	chdir(t, t.TempDir())
	t.Setenv("COSTBLAME_SERVER_API_TOKEN", "env-token")
	t.Setenv("ARGOCD_WEBHOOK_TOKEN", "argo-env")

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.APIToken != "env-token" {
		t.Errorf("api token = %q, want env-token (COSTBLAME_SERVER_API_TOKEN)", cfg.Server.APIToken)
	}
	if cfg.Sources.ArgoCD.Token != "argo-env" {
		t.Errorf("argocd token = %q, want argo-env (ARGOCD_WEBHOOK_TOKEN)", cfg.Sources.ArgoCD.Token)
	}
}

func TestLoad_DeployWorkflows(t *testing.T) {
	t.Run("from yaml list", func(t *testing.T) {
		dir := t.TempDir()
		yaml := "sources:\n  github:\n    deploy_workflows:\n      - Deploy\n      - release.yml\n"
		if err := os.WriteFile(filepath.Join(dir, "costblame.yaml"), []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		chdir(t, dir)
		cfg, err := Load("")
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.Sources.GitHub.DeployWorkflows; len(got) != 2 || got[0] != "Deploy" || got[1] != "release.yml" {
			t.Errorf("deploy_workflows = %q, want [Deploy release.yml]", got)
		}
	})

	t.Run("from comma-separated env var", func(t *testing.T) {
		chdir(t, t.TempDir())
		t.Setenv("COSTBLAME_SOURCES_GITHUB_DEPLOY_WORKFLOWS", "Deploy,Release")
		cfg, err := Load("")
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.Sources.GitHub.DeployWorkflows; len(got) != 2 || got[0] != "Deploy" || got[1] != "Release" {
			t.Errorf("deploy_workflows = %q, want [Deploy Release]", got)
		}
	})

	t.Run("unset means empty", func(t *testing.T) {
		chdir(t, t.TempDir())
		cfg, err := Load("")
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.Sources.GitHub.DeployWorkflows) != 0 {
			t.Errorf("deploy_workflows = %q, want empty", cfg.Sources.GitHub.DeployWorkflows)
		}
	})
}

package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestLoad_AnomalyDetectionSettings(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		chdir(t, t.TempDir())
		cfg, err := Load("")
		if err != nil {
			t.Fatal(err)
		}
		c := cfg.Cost
		if c.ZScoreThreshold != 3.5 || c.MinHistoryDays != 7 || c.SameWeekdayBaseline || c.SigmaFloorUSD != 1.0 {
			t.Errorf("defaults = z %v, min history %d, same weekday %v, floor %v; want 3.5, 7, false, 1.0",
				c.ZScoreThreshold, c.MinHistoryDays, c.SameWeekdayBaseline, c.SigmaFloorUSD)
		}
	})

	t.Run("from env", func(t *testing.T) {
		chdir(t, t.TempDir())
		t.Setenv("COSTBLAME_COST_ZSCORE_THRESHOLD", "5")
		t.Setenv("COSTBLAME_COST_MIN_HISTORY_DAYS", "14")
		t.Setenv("COSTBLAME_COST_SAME_WEEKDAY_BASELINE", "true")
		t.Setenv("COSTBLAME_COST_SIGMA_FLOOR_USD", "2.5")
		cfg, err := Load("")
		if err != nil {
			t.Fatal(err)
		}
		c := cfg.Cost
		if c.ZScoreThreshold != 5 || c.MinHistoryDays != 14 || !c.SameWeekdayBaseline || c.SigmaFloorUSD != 2.5 {
			t.Errorf("env overrides not applied: %+v", c)
		}
	})
}

func TestLoad_RescoreWindow(t *testing.T) {
	chdir(t, t.TempDir())
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Correlation.RescoreWindow; got != 24*time.Hour {
		t.Errorf("default rescore_window = %v, want 24h", got)
	}

	t.Setenv("COSTBLAME_CORRELATION_RESCORE_WINDOW", "6h")
	cfg, err = Load("")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Correlation.RescoreWindow; got != 6*time.Hour {
		t.Errorf("rescore_window from env = %v, want 6h", got)
	}
}

func TestLoad_ServiceMapAndPathPatterns(t *testing.T) {
	dir := t.TempDir()
	yaml := `
correlation:
  service_map:
    - match: acme/payments-*
      services: [AWS Lambda, DynamoDB]
    - match: payments
      services: [rds]
  path_patterns:
    - pattern: services/billing/
      service: Amazon DynamoDB
`
	if err := os.WriteFile(filepath.Join(dir, "costblame.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	chdir(t, dir)
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Correlation
	if len(c.ServiceMap) != 2 || c.ServiceMap[0].Match != "acme/payments-*" || len(c.ServiceMap[0].Services) != 2 || c.ServiceMap[1].Services[0] != "rds" {
		t.Errorf("service_map = %+v", c.ServiceMap)
	}
	if len(c.PathPatterns) != 1 || c.PathPatterns[0].Pattern != "services/billing/" || c.PathPatterns[0].Service != "Amazon DynamoDB" {
		t.Errorf("path_patterns = %+v", c.PathPatterns)
	}
}

func TestLoad_GitLabTokenAndBaseURL(t *testing.T) {
	chdir(t, t.TempDir())
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sources.GitLab.BaseURL != "https://gitlab.com" || cfg.Sources.GitLab.Token != "" {
		t.Errorf("defaults = %+v", cfg.Sources.GitLab)
	}

	t.Setenv("GITLAB_TOKEN", "glpat-env")
	t.Setenv("COSTBLAME_SOURCES_GITLAB_BASE_URL", "https://git.corp.example")
	cfg, err = Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sources.GitLab.Token != "glpat-env" || cfg.Sources.GitLab.BaseURL != "https://git.corp.example" {
		t.Errorf("env overrides not applied: %+v", cfg.Sources.GitLab)
	}
}

func TestLoad_AWSTagKeys(t *testing.T) {
	dir := t.TempDir()
	yaml := "cost:\n  aws:\n    tag_keys:\n      team: Team\n      env: Environment\n"
	if err := os.WriteFile(filepath.Join(dir, "costblame.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	chdir(t, dir)
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Cost.AWS.TagKeys; len(got) != 2 || got["team"] != "Team" || got["env"] != "Environment" {
		t.Errorf("tag_keys = %v", got)
	}

	chdir(t, t.TempDir())
	cfg, err = Load("")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Cost.AWS.TagKeys) != 0 {
		t.Errorf("tag attribution must be off by default, got %v", cfg.Cost.AWS.TagKeys)
	}
}

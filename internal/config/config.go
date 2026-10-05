package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Config holds all runtime configuration for costblame.
// Values are loaded from environment variables, a YAML config file, or CLI flags,
// in that precedence order (env wins).
//
// costblame is provider-agnostic: each section selects an adapter via a
// `provider` field (or by which sub-sections are filled in), and adapter-specific
// settings live in a matching sub-section. Built-in adapters are listed in the
// comments below; new ones plug in by implementing the relevant interface.
type Config struct {
	// Server controls the HTTP listener for the REST API and webhook receiver.
	Server ServerConfig `mapstructure:"server"`

	// Database selects the storage backend.
	Database DatabaseConfig `mapstructure:"database"`

	// Cost configures the cloud billing source (anomaly detection input).
	Cost CostConfig `mapstructure:"cost"`

	// Sources configures deploy event ingestion from CI/CD systems.
	Sources SourcesConfig `mapstructure:"sources"`

	// LLM configures AI narrative generation. Optional — costblame falls back
	// to template narratives when no provider is configured.
	LLM LLMConfig `mapstructure:"llm"`

	// Notify configures blame alert delivery destinations.
	Notify NotifyConfig `mapstructure:"notify"`

	// Correlation controls the engine's scoring behaviour.
	Correlation CorrelationConfig `mapstructure:"correlation"`
}

type ServerConfig struct {
	Host string `mapstructure:"host"`
	Port int    `mapstructure:"port"`
	// APIToken protects the web UI and /api. Send it as a bearer token, or as
	// the password in HTTP Basic auth (any username). When empty, `serve`
	// generates a random token for the run and logs it. Webhook receivers are
	// not covered — they verify their own signatures.
	APIToken string `mapstructure:"api_token"`
}

func (s ServerConfig) Addr() string {
	return fmt.Sprintf("%s:%d", s.Host, s.Port)
}

type DatabaseConfig struct {
	Driver string `mapstructure:"driver"` // "sqlite" or "postgres"
	DSN    string `mapstructure:"dsn"`
}

// CostConfig configures the cloud billing source. Provider selects the adapter
// (built-in: "aws"); adapter-specific settings live in the matching sub-section.
// New providers plug in by implementing collect.CostSource.
type CostConfig struct {
	Provider        string        `mapstructure:"provider"`
	PollInterval    time.Duration `mapstructure:"poll_interval"`
	LookbackDays    int           `mapstructure:"lookback_days"`         // baseline window for z-score
	AnomalyMinDelta float64       `mapstructure:"anomaly_min_delta_pct"` // minimum % increase to flag
	// ZScoreThreshold is the robust (median/MAD) z-score above which spend is
	// anomalous. 3.5 is the conventional cut-off for MAD-based scores.
	ZScoreThreshold float64 `mapstructure:"zscore_threshold"`
	// MinHistoryDays is how many days with spend a service needs before it can
	// be flagged; with less it is still "learning". 0 disables the check.
	MinHistoryDays int `mapstructure:"min_history_days"`
	// SameWeekdayBaseline compares a day with the same weekday only, so weekly
	// patterns (a Saturday batch job) are not flagged.
	SameWeekdayBaseline bool `mapstructure:"same_weekday_baseline"`
	// SigmaFloorUSD is the smallest spread ever used when scoring, so tiny
	// services do not flag on pennies.
	SigmaFloorUSD float64       `mapstructure:"sigma_floor_usd"`
	AWS           AWSCostConfig `mapstructure:"aws"`
}

// AWSCostConfig holds settings specific to the AWS Cost Explorer adapter.
type AWSCostConfig struct {
	Region      string `mapstructure:"region"`
	Granularity string `mapstructure:"granularity"` // "DAILY" or "HOURLY"
}

// SourcesConfig configures deploy event sources. Each filled-in sub-section
// enables one adapter — leave a section empty to disable it. New sources plug
// in by implementing collect.DeploySource.
type SourcesConfig struct {
	GitHub GitHubSourceConfig `mapstructure:"github"`
	GitLab GitLabSourceConfig `mapstructure:"gitlab"`
	ArgoCD ArgoCDSourceConfig `mapstructure:"argocd"`
}

// GitHubSourceConfig holds settings for the GitHub Actions webhook adapter.
// Enabled when webhook_secret is set (or GITHUB_WEBHOOK_SECRET is present);
// the secret is required because unsigned webhooks would be forgeable. token
// (or GITHUB_TOKEN) additionally enables PR/changed-files enrichment.
type GitHubSourceConfig struct {
	WebhookSecret string `mapstructure:"webhook_secret"`
	Token         string `mapstructure:"token"`
	// DeployWorkflows lists the workflows that count as deploys, matched
	// case-insensitively (with `*` globs) against each run's workflow name and
	// file path, e.g. ["Deploy", "release.yml"]. Empty treats every successful
	// push-triggered run on a production branch as a deploy — one per commit —
	// which over-counts if CI and deploy are separate workflows. Env:
	// COSTBLAME_SOURCES_GITHUB_DEPLOY_WORKFLOWS="Deploy,Release".
	DeployWorkflows []string `mapstructure:"deploy_workflows"`
	// PollInterval, when non-zero, enables fallback API polling instead of webhooks.
	PollInterval time.Duration `mapstructure:"poll_interval"`
}

// GitLabSourceConfig holds settings for the GitLab CI webhook adapter.
// Enabled when webhook_secret is set (or GITLAB_WEBHOOK_SECRET is present).
type GitLabSourceConfig struct {
	WebhookSecret string `mapstructure:"webhook_secret"`
	// Token is a GitLab access token with read_api scope. With it, each deploy's
	// changed files are fetched from the commit's diff so services can be
	// inferred from them. Without it, only correlation.service_map applies.
	Token string `mapstructure:"token"`
	// BaseURL is the GitLab root for self-managed instances
	// (default https://gitlab.com).
	BaseURL string `mapstructure:"base_url"`
}

// ArgoCDSourceConfig holds settings for the ArgoCD webhook adapter. It is an
// explicit opt-in and requires a token: ArgoCD's notifications webhook can send
// it as an `Authorization: Bearer <token>` header. Without one, anyone who can
// reach the endpoint could inject fake deploys and frame an author.
type ArgoCDSourceConfig struct {
	Enabled bool   `mapstructure:"enabled"`
	Token   string `mapstructure:"token"`
}

// LLMConfig configures narrative generation. Provider selects the adapter:
//
//   - "anthropic" — the Anthropic API (key from api_key or ANTHROPIC_API_KEY)
//   - "openai"    — OpenAI or any OpenAI-compatible endpoint via base_url
//     (Groq, Mistral, OpenRouter, vLLM, LM Studio, ...)
//   - "ollama"    — a local Ollama server (base_url or OLLAMA_HOST)
//   - "none"      — template narratives, no LLM calls
//   - ""          — auto-detect from whichever of the above env vars is set;
//     falls back to template narratives when none are
//
// New providers plug in by implementing correlate.NarrativeGenerator.
type LLMConfig struct {
	Provider string `mapstructure:"provider"`
	APIKey   string `mapstructure:"api_key"`
	Model    string `mapstructure:"model"`    // adapter default used when empty
	BaseURL  string `mapstructure:"base_url"` // for OpenAI-compatible / local endpoints
}

// NotifyConfig configures alert destinations. Every filled-in sub-section is
// enabled and alerts fan out to all of them. New destinations plug in by
// implementing notify.Notifier.
type NotifyConfig struct {
	Slack   SlackConfig         `mapstructure:"slack"`
	Webhook WebhookNotifyConfig `mapstructure:"webhook"`
}

type SlackConfig struct {
	BotToken string `mapstructure:"bot_token"`
	Channel  string `mapstructure:"channel"`
	// MinConfidence is the threshold above which alerts are sent.
	MinConfidence float64 `mapstructure:"min_confidence"`
}

// WebhookNotifyConfig configures the generic outbound webhook notifier —
// the escape hatch for any chat/incident/automation tool not built in.
type WebhookNotifyConfig struct {
	URL           string  `mapstructure:"url"`
	MinConfidence float64 `mapstructure:"min_confidence"`
}

type CorrelationConfig struct {
	// Interval is how often the correlation engine scores new anomalies.
	// Independent of cost.poll_interval: Cost Explorer polls cost money per
	// API call and can run slowly, while correlation is a cheap local query.
	Interval time.Duration `mapstructure:"interval"`
	// LookbackWindow is how far back to search for causative deploys.
	LookbackWindow time.Duration `mapstructure:"lookback_window"`
	// HighConfidenceThreshold triggers immediate narrative generation and alerting.
	HighConfidenceThreshold float64 `mapstructure:"high_confidence_threshold"`
	// RescoreWindow is how long after first scoring an anomaly keeps being
	// re-scored, so deploys and PR enrichment that arrive late are still weighed.
	// 0 disables re-scoring.
	RescoreWindow time.Duration `mapstructure:"rescore_window"`
	// ServiceMap assigns services to every deploy from a repository or ArgoCD
	// application, so sources that cannot list changed files can still match a
	// spiking service. YAML only.
	ServiceMap []ServiceMapEntry `mapstructure:"service_map"`
	// PathPatterns adds file-path rules, checked before the built-in ones. YAML only.
	PathPatterns []PathPatternEntry `mapstructure:"path_patterns"`
	// MinScoreToStore discards edges below this score to keep the DB clean.
	MinScoreToStore float64 `mapstructure:"min_score_to_store"`
}

// ServiceMapEntry maps a repository path, repository URL or ArgoCD application
// name (glob, case-insensitive; "*" matches any run of characters) to services.
type ServiceMapEntry struct {
	Match    string   `mapstructure:"match"`
	Services []string `mapstructure:"services"`
}

// PathPatternEntry maps changed files whose path contains Pattern to a service.
type PathPatternEntry struct {
	Pattern string `mapstructure:"pattern"`
	Service string `mapstructure:"service"`
}

// Load reads configuration from environment variables and an optional config file.
// Environment variables are mapped as COSTBLAME_<SECTION>_<KEY> (e.g. COSTBLAME_COST_PROVIDER,
// COSTBLAME_LLM_API_KEY, COSTBLAME_SOURCES_GITHUB_WEBHOOK_SECRET).
func Load(cfgFile string) (*Config, error) {
	v := viper.New()

	v.SetDefault("server.host", "0.0.0.0")
	v.SetDefault("server.port", 8080)
	v.SetDefault("server.api_token", "")
	v.SetDefault("database.driver", "sqlite")
	v.SetDefault("database.dsn", "costblame.db")
	v.SetDefault("cost.provider", "aws")
	v.SetDefault("cost.poll_interval", "15m")
	v.SetDefault("cost.lookback_days", 30)
	v.SetDefault("cost.anomaly_min_delta_pct", 20.0)
	v.SetDefault("cost.zscore_threshold", 3.5)
	v.SetDefault("cost.min_history_days", 7)
	v.SetDefault("cost.same_weekday_baseline", false)
	v.SetDefault("cost.sigma_floor_usd", 1.0)
	v.SetDefault("cost.aws.region", "us-east-1")
	v.SetDefault("cost.aws.granularity", "DAILY")
	v.SetDefault("sources.github.webhook_secret", "")
	v.SetDefault("sources.github.token", "")
	v.SetDefault("sources.github.deploy_workflows", []string{})
	v.SetDefault("sources.gitlab.webhook_secret", "")
	v.SetDefault("sources.gitlab.token", "")
	v.SetDefault("sources.gitlab.base_url", "https://gitlab.com")
	v.SetDefault("sources.argocd.enabled", false)
	v.SetDefault("sources.argocd.token", "")
	v.SetDefault("llm.provider", "") // empty = auto-detect from environment
	v.SetDefault("llm.api_key", "")
	v.SetDefault("llm.model", "")
	v.SetDefault("llm.base_url", "")
	v.SetDefault("notify.slack.bot_token", "")
	v.SetDefault("notify.slack.channel", "")
	v.SetDefault("notify.slack.min_confidence", 0.65)
	v.SetDefault("notify.webhook.url", "")
	v.SetDefault("notify.webhook.min_confidence", 0.65)
	v.SetDefault("correlation.interval", "1m")
	v.SetDefault("correlation.lookback_window", "72h")
	v.SetDefault("correlation.high_confidence_threshold", 0.65)
	v.SetDefault("correlation.min_score_to_store", 0.10)
	v.SetDefault("correlation.rescore_window", "24h")

	v.SetEnvPrefix("COSTBLAME")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	if cfgFile != "" {
		v.SetConfigFile(cfgFile)
	} else {
		// No SetConfigType here: with an explicit type viper also tries the bare
		// name "costblame", which in a release archive is the binary itself.
		v.SetConfigName("costblame")
		v.AddConfigPath(".")
		v.AddConfigPath("$HOME/.costblame")
		v.AddConfigPath("/etc/costblame")
	}

	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("reading config: %w", err)
		}
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshalling config: %w", err)
	}

	applyEnvFallbacks(&cfg)

	return &cfg, nil
}

// applyEnvFallbacks fills empty config fields from the conventional environment
// variables each tool's ecosystem already uses, so costblame picks up whichever
// integrations are present without any YAML at all. Explicit config
// (COSTBLAME_* env vars or the YAML file) always wins.
func applyEnvFallbacks(cfg *Config) {
	fallback := func(target *string, envKeys ...string) {
		if *target != "" {
			return
		}
		for _, key := range envKeys {
			if v := os.Getenv(key); v != "" {
				*target = v
				return
			}
		}
	}

	fallback(&cfg.Sources.GitHub.WebhookSecret, "GITHUB_WEBHOOK_SECRET")
	fallback(&cfg.Sources.GitHub.Token, "GITHUB_TOKEN")
	fallback(&cfg.Sources.GitLab.WebhookSecret, "GITLAB_WEBHOOK_SECRET")
	fallback(&cfg.Sources.GitLab.Token, "GITLAB_TOKEN")
	fallback(&cfg.Sources.ArgoCD.Token, "ARGOCD_WEBHOOK_TOKEN")
	fallback(&cfg.Notify.Slack.BotToken, "SLACK_BOT_TOKEN")
	fallback(&cfg.Notify.Slack.Channel, "SLACK_CHANNEL")
	// LLM provider env vars (ANTHROPIC_API_KEY, OPENAI_API_KEY, OLLAMA_HOST, …)
	// are resolved by the narrative generator builder, since which one applies
	// depends on the selected/detected provider.
}

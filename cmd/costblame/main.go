// costblame — blame the right deploy for your cloud cost spike.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/prem0x01/costblame/internal/api"
	"github.com/prem0x01/costblame/internal/collect"
	argocdcollect "github.com/prem0x01/costblame/internal/collect/argocd"
	"github.com/prem0x01/costblame/internal/collect/aws"
	githubcollect "github.com/prem0x01/costblame/internal/collect/github"
	gitlabcollect "github.com/prem0x01/costblame/internal/collect/gitlab"
	"github.com/prem0x01/costblame/internal/config"
	"github.com/prem0x01/costblame/internal/correlate"
	"github.com/prem0x01/costblame/internal/narrative"
	"github.com/prem0x01/costblame/internal/notify"
	slacknotify "github.com/prem0x01/costblame/internal/notify/slack"
	webhooknotify "github.com/prem0x01/costblame/internal/notify/webhook"
	sqlitestore "github.com/prem0x01/costblame/internal/store/sqlite"
	"github.com/prem0x01/costblame/internal/tui"
	"github.com/prem0x01/costblame/internal/web"
)

var cfgFile string

func main() {
	root := &cobra.Command{
		Use:   "costblame",
		Short: "Correlate cloud cost spikes with the deployments that caused them",
	}

	root.PersistentFlags().StringVarP(&cfgFile, "config", "c", "", "config file (default: costblame.yaml)")

	root.AddCommand(
		serveCmd(),
		migrateCmd(),
		reportCmd(),
		tuiCmd(),
	)

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

// migrateCmd applies all pending database migrations and exits.
// Designed to run as a Docker init container before the main service starts.
func migrateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "Apply database migrations and exit",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgFile)
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			store, err := sqlitestore.New(cfg.Database.DSN)
			if err != nil {
				return fmt.Errorf("opening store: %w", err)
			}
			defer store.Close()

			if err := store.Migrate(context.Background()); err != nil {
				return fmt.Errorf("migrations failed: %w", err)
			}
			slog.Info("migrations applied successfully")
			return nil
		},
	}
}

// serveCmd starts the full daemon: cost poller, deploy event receiver, correlation engine,
// and HTTP API server all running concurrently.
func serveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Start the costblame daemon (poller + webhook receiver + correlation engine + API)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgFile)
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}

			store, err := sqlitestore.New(cfg.Database.DSN)
			if err != nil {
				return fmt.Errorf("opening store: %w", err)
			}
			defer store.Close()

			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			if err := store.Migrate(ctx); err != nil {
				return fmt.Errorf("running migrations: %w", err)
			}

			// Build notifier chain — every configured destination gets the alert.
			notifier := buildNotifier(cfg.Notify)

			// Build narrative generator (falls back to templates if no LLM is configured).
			gen, narrativeMode, err := buildNarrativeGenerator(cfg.LLM)
			if err != nil {
				return err
			}

			// Build cost source for the configured provider.
			costSrc, err := buildCostSource(ctx, cfg.Cost)
			if err != nil {
				return err
			}

			// Build every deploy source with credentials configured.
			deploySources := buildDeploySources(cfg.Sources)
			if len(deploySources) == 0 {
				slog.Warn("no deploy sources configured — anomalies will be detected but cannot be blamed on deploys")
			}

			// Correlation engine.
			engine := correlate.NewEngine(
				store, gen, notifier,
				cfg.Correlation.Interval,
				cfg.Correlation.MinScoreToStore,
			)

			// Start background cost polling.
			go pollCosts(ctx, costSrc, store, cfg.Cost.PollInterval)

			// Start consuming deploy events from every configured source.
			webhooks := make(map[string]http.Handler, len(deploySources))
			for name, src := range deploySources {
				webhooks[name] = src
				go consumeDeployEvents(ctx, src, store)
			}

			// Start correlation engine.
			go engine.Run(ctx)

			// The web UI's system status reflects exactly what was built above —
			// nothing here is re-derived or guessed separately.
			deploySourceNames := make([]string, 0, len(deploySources))
			for name := range deploySources {
				deploySourceNames = append(deploySourceNames, name)
			}
			status := web.SystemStatus{
				CostProvider:    costSrc.Name(),
				DeploySources:   deploySourceNames,
				NarrativeEngine: narrativeMode,
			}

			// Start HTTP server (JSON API under /api, browser UI on the clean paths).
			apiToken, generated, err := resolveAPIToken(cfg.Server.APIToken)
			if err != nil {
				return err
			}
			if generated {
				// Shown once so a zero-config install is still locked down. It
				// changes on every restart; set server.api_token to pin it.
				slog.Warn("server.api_token not set — generated a temporary token for the web UI and /api. "+
					"Log in with any username and this password, or send it as a bearer token. "+
					"Set COSTBLAME_SERVER_API_TOKEN to keep it stable across restarts.",
					"token", apiToken)
			}
			srv := api.New(cfg.Server.Addr(), apiToken, store, webhooks, web.New(store, status))
			slog.Info("costblame started", "addr", cfg.Server.Addr())
			return srv.Start(ctx)
		},
	}
}

// reportCmd prints recent blame edges as a table to stdout. Useful for CI/CD pipelines
// or quick terminal checks without starting the full daemon.
func reportCmd() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Print recent blame edges as a table",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgFile)
			if err != nil {
				return err
			}
			store, err := sqlitestore.New(cfg.Database.DSN)
			if err != nil {
				return err
			}
			defer store.Close()

			edges, err := store.RecentBlameEdges(context.Background(), limit)
			if err != nil {
				return err
			}

			if len(edges) == 0 {
				fmt.Println("No blame edges found.")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "SERVICE\tDELTA\tSCORE\tPR\tAUTHOR\tSTATUS\tWHEN")
			for _, e := range edges {
				service, delta, pr, author, when := "(unknown)", "-", "-", "-", "-"
				if e.CostSnapshot != nil {
					service = e.CostSnapshot.Service
					delta = fmt.Sprintf("%+.1f%%", e.CostSnapshot.DeltaPct)
				}
				if e.DeployEvent != nil {
					pr = fmt.Sprintf("#%d", e.DeployEvent.PRNumber)
					author = "@" + e.DeployEvent.PRAuthor
					when = e.DeployEvent.OccurredAt.Format("Jan 2 15:04")
				}
				fmt.Fprintf(w, "%s\t%s\t%.0f%%\t%s\t%s\t%s\t%s\n",
					service, delta, e.ConfidenceScore*100, pr, author, string(e.Status), when)
			}
			return w.Flush()
		},
	}
	cmd.Flags().IntVarP(&limit, "limit", "n", 20, "number of edges to show")
	return cmd
}

// tuiCmd launches the interactive terminal UI.
func tuiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tui",
		Short: "Open the interactive blame timeline terminal UI",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgFile)
			if err != nil {
				return err
			}
			store, err := sqlitestore.New(cfg.Database.DSN)
			if err != nil {
				return err
			}
			defer store.Close()

			p := tea.NewProgram(tui.New(store), tea.WithAltScreen())
			_, err = p.Run()
			return err
		},
	}
}

// buildNotifier assembles the alert fan-out from every configured destination.
// Returns a no-op notifier when nothing is configured.
func buildNotifier(cfg config.NotifyConfig) notify.Notifier {
	var notifiers []notify.Notifier
	if cfg.Slack.BotToken != "" {
		notifiers = append(notifiers, slacknotify.New(cfg.Slack.BotToken, cfg.Slack.Channel, cfg.Slack.MinConfidence))
	}
	if cfg.Webhook.URL != "" {
		notifiers = append(notifiers, webhooknotify.New(cfg.Webhook.URL, cfg.Webhook.MinConfidence))
	}
	if len(notifiers) == 0 {
		return &notify.Noop{}
	}
	return notify.NewMulti(notifiers...)
}

// buildDeploySources returns every CI/CD adapter whose credentials are
// configured (via YAML, COSTBLAME_* env vars, or the conventional
// GITHUB_*/GITLAB_* env vars). Filling in a source is what enables it.
func buildDeploySources(cfg config.SourcesConfig) map[string]webhookSource {
	sources := make(map[string]webhookSource)
	switch {
	case cfg.GitHub.WebhookSecret != "":
		sources["github"] = githubcollect.NewWebhookHandler(cfg.GitHub.WebhookSecret, cfg.GitHub.Token, cfg.GitHub.DeployWorkflows)
		if len(cfg.GitHub.DeployWorkflows) == 0 {
			slog.Info("github: sources.github.deploy_workflows not set — every successful push-triggered " +
				"workflow run on main/release/deploy branches counts as a deploy (one per commit); " +
				"set it to your deploy workflow(s) for precise deploy times")
		}
	case cfg.GitHub.Token != "":
		// An HMAC computed with an empty key is trivially forgeable, so a
		// receiver without a secret would let anyone inject deploy events.
		slog.Warn("github token configured but sources.github.webhook_secret is empty — " +
			"refusing to mount the GitHub webhook receiver; set a webhook secret to enable it")
	}
	if cfg.GitLab.WebhookSecret != "" {
		sources["gitlab"] = gitlabcollect.New(cfg.GitLab.WebhookSecret)
	}
	switch {
	case cfg.ArgoCD.Enabled && cfg.ArgoCD.Token != "":
		sources["argocd"] = argocdcollect.New(cfg.ArgoCD.Token)
	case cfg.ArgoCD.Enabled:
		// Unauthenticated, the endpoint would let anyone inject fake deploys.
		slog.Warn("sources.argocd.enabled is true but sources.argocd.token is empty — " +
			"refusing to mount the ArgoCD webhook receiver; set a token and send it as " +
			"an `Authorization: Bearer <token>` header from ArgoCD's webhook notification")
	}
	for name := range sources {
		slog.Info("deploy source enabled", "source", name)
	}
	return sources
}

// resolveAPIToken returns the configured API token, or a freshly generated
// random one (generated=true) when none is set, so the UI and API are never
// left open by default.
func resolveAPIToken(configured string) (token string, generated bool, err error) {
	if configured != "" {
		return configured, false, nil
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", false, fmt.Errorf("generating API token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), true, nil
}

// webhookSource is a deploy source that receives its events over HTTP.
type webhookSource interface {
	collect.DeploySource
	http.Handler
}

// buildNarrativeGenerator selects the LLM adapter for the configured provider.
// With no provider set, it auto-detects from whichever conventional env var is
// present (ANTHROPIC_API_KEY, OPENAI_API_KEY, OLLAMA_HOST). A missing key
// degrades to template narratives instead of failing — costblame never
// requires an LLM to run. The returned string is the effective mode
// ("template", "anthropic", "openai", "ollama"), surfaced in the web UI's
// system status so an operator can see what's actually active.
func buildNarrativeGenerator(cfg config.LLMConfig) (correlate.NarrativeGenerator, string, error) {
	provider := cfg.Provider
	if provider == "" || provider == "auto" {
		provider = detectLLMProvider(cfg)
		if provider != "none" {
			slog.Info("LLM provider auto-detected", "provider", provider)
		}
	}

	switch provider {
	case "none":
		slog.Info("no LLM configured — using template narratives")
		return &narrative.NoopGenerator{}, "template", nil

	case "anthropic":
		key := firstNonEmpty(cfg.APIKey, os.Getenv("ANTHROPIC_API_KEY"))
		if key == "" {
			slog.Warn("llm.provider is anthropic but no API key found — using template narratives")
			return &narrative.NoopGenerator{}, "template", nil
		}
		return narrative.New(key, cfg.Model), "anthropic", nil

	case "openai":
		key := firstNonEmpty(cfg.APIKey, os.Getenv("OPENAI_API_KEY"))
		base := firstNonEmpty(cfg.BaseURL, os.Getenv("OPENAI_BASE_URL"), "https://api.openai.com/v1")
		if key == "" && base == "https://api.openai.com/v1" {
			slog.Warn("llm.provider is openai but no API key found — using template narratives")
			return &narrative.NoopGenerator{}, "template", nil
		}
		model := cfg.Model
		if model == "" {
			if base != "https://api.openai.com/v1" {
				return nil, "", fmt.Errorf("llm.model is required when using a custom base_url (%s)", base)
			}
			model = "gpt-4o-mini"
		}
		return narrative.NewOpenAICompatible(base, key, model), "openai", nil

	case "ollama":
		base := firstNonEmpty(cfg.BaseURL, ollamaBaseURL(os.Getenv("OLLAMA_HOST")), "http://localhost:11434/v1")
		model := firstNonEmpty(cfg.Model, "llama3.1")
		return narrative.NewOpenAICompatible(base, cfg.APIKey, model), "ollama", nil

	default:
		return nil, "", fmt.Errorf("unknown llm provider %q — built-in: anthropic, openai, ollama, none; add your own by implementing correlate.NarrativeGenerator", cfg.Provider)
	}
}

// detectLLMProvider infers the provider from which credentials are present.
// Config fields win over env vars; the first match in order applies.
func detectLLMProvider(cfg config.LLMConfig) string {
	switch {
	case cfg.BaseURL != "":
		return "openai" // any OpenAI-compatible endpoint, hosted or local
	case cfg.APIKey != "":
		return "anthropic" // bare api_key keeps the historical default
	case os.Getenv("ANTHROPIC_API_KEY") != "":
		return "anthropic"
	case os.Getenv("OPENAI_API_KEY") != "" || os.Getenv("OPENAI_BASE_URL") != "":
		return "openai"
	case os.Getenv("OLLAMA_HOST") != "":
		return "ollama"
	default:
		return "none"
	}
}

// ollamaBaseURL converts an OLLAMA_HOST value ("http://127.0.0.1:11434") into
// the OpenAI-compatible endpoint Ollama serves under /v1.
func ollamaBaseURL(host string) string {
	if host == "" {
		return ""
	}
	host = strings.TrimRight(host, "/")
	if !strings.HasPrefix(host, "http://") && !strings.HasPrefix(host, "https://") {
		host = "http://" + host
	}
	if !strings.HasSuffix(host, "/v1") {
		host += "/v1"
	}
	return host
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// buildCostSource selects the billing adapter for the configured provider.
func buildCostSource(ctx context.Context, cfg config.CostConfig) (collect.CostSource, error) {
	switch cfg.Provider {
	case "", "aws":
		src, err := aws.NewCESource(ctx,
			cfg.AWS.Region, cfg.AWS.Granularity, cfg.LookbackDays,
			aws.Detector{
				ZThreshold:    cfg.ZScoreThreshold,
				MinDeltaPct:   cfg.AnomalyMinDelta,
				MinHistory:    cfg.MinHistoryDays,
				SameWeekday:   cfg.SameWeekdayBaseline,
				SigmaFloorUSD: cfg.SigmaFloorUSD,
			},
		)
		if err != nil {
			return nil, fmt.Errorf("aws cost source: %w", err)
		}
		return src, nil
	default:
		return nil, fmt.Errorf("unknown cost provider %q — built-in: aws; add your own by implementing collect.CostSource", cfg.Provider)
	}
}

// pollCosts runs the cost collection loop on a ticker.
func pollCosts(ctx context.Context, src collect.CostSource, store *sqlitestore.Store, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	collect := func() {
		// Collect the most recent complete UTC day. Day-aligned boundaries keep
		// the snapshot's natural key stable across polls, so re-polls upsert the
		// same row (refreshing amounts as Cost Explorer data settles) instead of
		// inserting duplicates that would be re-blamed and re-alerted.
		to := time.Now().UTC().Truncate(24 * time.Hour)
		from := to.Add(-24 * time.Hour)
		snaps, err := src.Collect(ctx, from, to)
		if err != nil {
			slog.Error("cost collection failed", "err", err)
			return
		}
		if err := store.SaveCostSnapshots(ctx, snaps); err != nil {
			slog.Error("saving cost snapshots", "err", err)
		}
		slog.Info("cost snapshots collected", "count", len(snaps))
	}
	collect() // run immediately on start
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			collect()
		}
	}
}

// consumeDeployEvents reads from the deploy source channel and persists events.
func consumeDeployEvents(ctx context.Context, src collect.DeploySource, store *sqlitestore.Store) {
	events, err := src.Events(ctx)
	if err != nil {
		slog.Error("opening deploy event stream", "err", err)
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-events:
			if !ok {
				return
			}
			if err := store.UpsertDeployEvent(ctx, event); err != nil {
				slog.Error("persisting deploy event", "err", err)
			}
		}
	}
}

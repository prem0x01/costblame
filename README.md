<div align="center">

# 💸 costblame

### `git blame`, but for your cloud bill.

**Every cost spike has a name on it. costblame finds it in seconds.**

[![Go 1.22+](https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)
[![Self-hosted](https://img.shields.io/badge/self--hosted-your%20infra%2C%20your%20data-blueviolet)](#faq)
[![LLM](https://img.shields.io/badge/LLM-optional%20%C2%B7%20any%20provider%20%C2%B7%20local%20OK-orange)](#narratives-any-llm-or-none)
[![Storage](https://img.shields.io/badge/storage-SQLite%20embedded-lightgrey?logo=sqlite)](#architecture--the-guided-tour)

</div>

---

Your bill spiked at 3am. Somebody's deploy did it. `costblame` tells you **which deploy, which change, and whose name is on it** — with a confidence score and a plain-English explanation, delivered to your team before anyone opens a billing dashboard.

One self-hosted Go binary. It watches your cloud spend, listens to your CI/CD pipeline, and runs a multi-factor scoring engine that connects cost anomalies to the deployments most likely to have caused them. No SaaS. No agents on your hosts. No data leaving your infrastructure.

```
 you            : "why is the bill up 145%?"
 costblame      : "PR #847 by @alice — provisioned concurrency on the
                   payment function, deployed 6h before the spike.
                   Confidence 0.79. Here's the revert suggestion."
 time elapsed   : 4 seconds
 dashboards open: 0
```

---

## Table of Contents

- [The Problem](#the-problem)
- [How It Works](#how-it-works)
- [Bring Your Own Everything](#bring-your-own-everything)
- [Architecture — the Guided Tour](#architecture--the-guided-tour)
- [Confidence Scoring](#confidence-scoring)
- [Anatomy of a Blame](#anatomy-of-a-blame)
- [Use Cases](#use-cases)
- [Quickstart](#quickstart)
- [CLI Reference](#cli-reference)
- [Configuration](#configuration)
- [REST API](#rest-api)
- [Adapter Setup](#adapter-setup)
- [Extending costblame](#extending-costblame)
- [FAQ](#faq)
- [Testing](#testing) · [Technology Stack](#technology-stack) · [License](#license)

---

## The Problem

Cloud bills spike. Everyone has seen it: serverless invocations explode overnight, a compute fleet doubles, a database starts scanning instead of querying. You open your billing dashboard and see the spike. Then the investigation begins.

| | Without costblame | With costblame |
|---|---|---|
| **Detection** | Someone notices the bill — days later | Anomaly flagged on the next poll, minutes after billing data lands |
| **Investigation** | 30–90 min of cross-referencing deploy logs, Git history, billing dashboards | Automatic — scored candidates ranked by evidence |
| **The answer** | "Probably something we shipped last week?" | *"PR #847 by @alice, 6h before the spike, confidence 0.79"* |
| **The follow-up** | A meeting | A Slack message with a suggested fix, and a one-click confirm/dismiss |
| **Next time** | Start from scratch | Confirmed blames sharpen future scoring |

That investigation tax is paid on *every single spike*. `costblame` pays it once — in code.

---

## How It Works

Six steps, fully automatic:

1. **Polls your cloud billing API** on a configurable interval. For each service, it computes a rolling 30-day baseline and flags anything more than 2 standard deviations above it as an anomaly.
2. **Ingests deploy events** from your CI/CD via webhooks, enriching each one asynchronously with PR metadata and changed file paths.
3. **Scores every deployment** in the 72-hour window before each anomaly on four independent, explainable factors. The weighted sum is the confidence score — no black box.
4. **Generates a narrative** — a 2–3 sentence plain-English explanation of what spiked, what probably caused it, and what to do next. Uses your LLM if you configure one, clean templates if you don't. **An LLM is never required.**
5. **Alerts your team** through every notifier you've configured — chat, incident tooling, or any HTTP endpoint.
6. **Takes feedback** through a REST API and terminal UI: confirm a blame and future scoring gets sharper; dismiss it and the false positive is remembered.

---

## Bring Your Own Everything

`costblame` is built around four small Go interfaces. Every external system is an adapter — use the built-in ones, or write your own in an afternoon.

| Slot | Interface | Built-in adapters | Swap in |
|------|-----------|-------------------|---------|
| 💸 Billing source | `collect.CostSource` | AWS Cost Explorer | Any cloud or FinOps platform with a cost API |
| 🚀 Deploy events | `collect.DeploySource` | GitHub Actions · GitLab CI · ArgoCD | Any CI/CD that can send a webhook |
| 🧠 Narratives | `correlate.NarrativeGenerator` | Anthropic · OpenAI · Ollama (local) · any OpenAI-compatible endpoint · template fallback | Any LLM provider — or none |
| 📣 Alerts | `notify.Notifier` | Slack · generic outbound webhook | Anything that accepts an HTTP POST |

The correlation engine — the part that actually answers *"who did this?"* — depends only on these interfaces. It doesn't know or care which cloud, which CI, or which model you use.

### Filled in = switched on

You don't enumerate integrations anywhere — costblame inspects what's configured and acts accordingly. Every deploy source with a secret gets a webhook endpoint; every alert destination with credentials receives alerts; the LLM provider is auto-detected from whichever conventional env var is present:

```bash
GITHUB_WEBHOOK_SECRET=…   # → /webhooks/github goes live
GITLAB_WEBHOOK_SECRET=…   # → /webhooks/gitlab goes live
ANTHROPIC_API_KEY=…       # → narratives via the Anthropic API
OPENAI_API_KEY=…          # → …or via OpenAI
OLLAMA_HOST=…             # → …or via your local model. None set? Templates.
SLACK_BOT_TOKEN=…         # → alerts to Slack
```

That means a fully working deployment can be **zero YAML** — environment variables alone are enough.

---

## Architecture — the Guided Tour

The whole system is one process. Data flows top to bottom: the outside world feeds two collectors, everything meets in an embedded store, the correlation engine turns raw events into blame, and the results flow back out to humans and machines.

```
                       T H E   O U T S I D E   W O R L D

        cloud billing API                CI/CD pipelines          your team
       any provider with a          GitHub · GitLab · ArgoCD   chat · webhooks
     cost API (AWS built-in)         anything that can POST    API · terminal
                │                               │        (6) alerts & ▲
                │ (1) pull · every 15m          │ (2) push    answers │
                ▼                               ▼                     │
╔══════════════════════════════════════════════════════════════════════════════╗
║ COSTBLAME · one process · one binary · your infrastructure          ▲        ║
║                                                                     │        ║
║  ┌─────────────────────────┐  ┌──────────────────────────────┐      │        ║
║  │ COST COLLECTOR          │  │ DEPLOY RECEIVERS             │      │        ║
║  │ collect.CostSource      │  │ collect.DeploySource × N     │      │        ║
║  │                         │  │                              │      │        ║
║  │ · rolling 30-day        │  │ · POST /webhooks/<source>    │      │        ║
║  │   baseline per service  │  │ · signature check, then      │      │        ║
║  │ · z-score > 2σ  AND     │  │   202 Accepted in ~1 ms      │      │        ║
║  │   Δ ≥ 20%  ⇒  anomaly   │  │ · PR + changed files         │      │        ║
║  │                         │  │   enriched in background     │      │        ║
║  └────────────┬────────────┘  └───────────────┬──────────────┘      │        ║
║               │ CostSnapshot                  │ DeployEvent         │        ║
║               ▼                               ▼                     │        ║
║  ┌───────────────────────────────────────────────────────────┐      │        ║
║  │ (3) EMBEDDED STORE · SQLite · WAL · zero infra to operate │      │        ║
║  │                                                           │      │        ║
║  │      cost_snapshots ◄──── blame_edges ────► deploy_events │      │        ║
║  │               (the blame graph lives here)                │      │        ║
║  └─────────────────────────────┬─────────────────────────────┘      │        ║
║                                │ every poll tick                    │        ║
║                                ▼                                    │        ║
║  ┌───────────────────────────────────────────────────────────┐      │        ║
║  │ (4) CORRELATION ENGINE — answers "who did this?"          │      │        ║
║  │                                                           │      │        ║
║  │ for each anomaly nobody has blamed yet:                   │      │        ║
║  │   · collect every deploy from the 72 h before the spike   │      │        ║
║  │   · score each (anomaly, deploy) pair on 4 factors:       │      │        ║
║  │       time 40% · service 30% · tags 20% · history 10%     │      │        ║
║  │   · <0.10 drop  ·  ≥0.10 keep as pending  ·  ≥0.65 act    │      │        ║
║  └─────────────────────────────┬─────────────────────────────┘      │        ║
║                                │ (5) high confidence ⇒ act          │        ║
║               ┌────────────────┴──────────────┐                     │        ║
║               ▼                               ▼                     │        ║
║  ┌─────────────────────────┐  ┌──────────────────────────────┐      │        ║
║  │ NARRATIVE GENERATOR     │  │ NOTIFIER FAN-OUT             │      │        ║
║  │ any LLM, hosted/local,  │─►│ notify.Notifier × N          │──────┤        ║
║  │ or template fallback    │  │ Slack · webhook · yours      │      │        ║
║  └─────────────────────────┘  └──────────────────────────────┘      │        ║
║                                                                     │        ║
║  ┌─────────────────────────────────────────────────────────────┐    │        ║
║  │ (6) REVIEW & FEEDBACK LOOP                                  │────┘        ║
║  │ REST API · /blame /anomalies /confirm /dismiss /healthz     │             ║
║  │ TUI · interactive blame timeline · costblame tui            │             ║
║  │                                                             │             ║
║  │ confirm ⇒ author+service history boosts future scores       │             ║
║  │ dismiss ⇒ false positive recorded, never re-alerted         │             ║
║  └─────────────────────────────────────────────────────────────┘             ║
╚══════════════════════════════════════════════════════════════════════════════╝
```

### ❶ The cost collector — *pull*

A `collect.CostSource` adapter polls your billing API on `cost.poll_interval` (default 15m). For every service it sees, it maintains a rolling 30-day baseline (mean and standard deviation of daily spend). A reading is flagged as an anomaly only when **both** conditions hold: the z-score exceeds 2.0 *and* the relative increase beats `anomaly_min_delta_pct` (default 20%). The double condition matters — the z-score catches statistically unusual jumps, while the minimum delta filters out "statistically unusual but who cares" noise on near-zero services. Anomalies land in the store as `CostSnapshot` rows with `is_anomaly = true`.

### ❷ The deploy receivers — *push*

Every configured `collect.DeploySource` gets its own endpoint at `POST /webhooks/<name>` on the shared HTTP server. Receivers follow a strict **two-phase pattern** designed to never make your CI/CD wait:

- **Phase 1 (synchronous, ~1ms):** verify the signature (HMAC-SHA256 for GitHub, token header for GitLab — both in constant time), then immediately return `202 Accepted`.
- **Phase 2 (background goroutine):** parse the payload, call back to the source API for PR title, author, labels, and changed file paths, infer which cloud services the change touches, and upsert the `DeployEvent`. The upsert merges enrichment into the existing row, so a slow PR-metadata fetch never loses the original event.

Events flow through a buffered channel; if a source floods faster than the store can absorb, costblame sheds load by dropping (and logging) rather than blocking your pipeline's webhook delivery.

### ❸ The embedded store

SQLite in WAL mode — chosen deliberately. costblame's write volume (a few rows per poll, one row per deploy) is trivially within SQLite's comfort zone, and an embedded store means the entire product is **one binary plus one file**. Nothing else to provision, patch, or pay for. Three tables carry everything: `cost_snapshots`, `deploy_events`, and `blame_edges` — the third being the actual blame graph, with each edge linking one anomaly to one deploy along with its score, factor breakdown, narrative, and status.

### ❹ The correlation engine

The brain. On every poll tick it asks one question: *are there anomalies nobody has blamed yet?* For each one it pulls every deploy in the preceding `lookback_window` (default 72h) and hands each `(anomaly, deploy)` pair to the scorer — a stateless, fully unit-tested function that returns four named factors and their weighted sum (see [Confidence Scoring](#confidence-scoring)). Then it triages by score: below `min_score_to_store` (0.10) the pair is discarded; up to `high_confidence_threshold` (0.65) it's stored as `pending` for humans to browse; at or above 0.65 the engine **acts** — step ❺.

### ❺ Narrative + alert

For high-confidence edges only, the engine asks the `NarrativeGenerator` for a 2–3 sentence explanation written for a human at 9am: what spiked, by how much, which deploy is implicated, and what to consider doing. The generator is whichever LLM adapter you configured — Anthropic, OpenAI, a local Ollama model, any OpenAI-compatible endpoint — or the built-in template that needs no network calls at all. The result fans out through every configured `notify.Notifier` simultaneously; a failing notifier is logged and skipped, never blocking the others.

### ❻ The feedback loop

This is what separates costblame from a dashboard: the system **learns from your verdicts**. Confirm an edge (API, or `enter` → confirm in the TUI) and that author/service pairing boosts the `historical_pattern` factor in every future investigation. Dismiss it and the false positive is recorded and never re-alerted. The blame graph becomes an institutional memory of which kinds of changes cost you money.

### Design choices worth knowing

- **Interfaces at every boundary.** The engine imports zero vendor SDKs. Each adapter lives in its own package and is selected at startup from config — which is why swapping clouds, CIs, or LLMs is a config change, not a fork.
- **No queue, no broker, no sidecar.** Goroutines and channels do the fan-in; SQLite does the durability. The deployment story stays: *run the binary*.
- **Fail open, degrade gracefully.** No LLM key? Templates. No notifier? Edges still stored and queryable. A webhook source floods? Shed load, keep serving.
- **stdlib HTTP** (Go 1.22 pattern routing) — no web framework to keep up with.

---

## Confidence Scoring

Every `(anomaly, deployment)` pair is scored by four independent factors. The weighted sum is the confidence score, capped at 1.0. **All four factors are visible on every blame edge** — nothing is hidden behind a model.

| Factor | Weight | How it's calculated |
|--------|--------|---------------------|
| `temporal_proximity` | **40%** | Decay curve: 1.0 if the deploy was within 2h of the spike, 0.85 within 6h, 0.60 within 24h, 0.30 within 48h, 0.10 within 72h, 0.0 beyond |
| `service_match` | **30%** | 1.0 if the deploy's inferred cloud services (from changed file paths) exactly match the anomalous service; 0.7 for a partial match; 0.0 for no match |
| `tag_match` | **20%** | Compares cost allocation tags (`team`, `env`) on the anomaly against the deployment's repository and environment metadata |
| `historical_pattern` | **10%** | Boosts the score if the same author has caused *confirmed* cost spikes on the same service before (capped at 4 prior incidents) |

**Thresholds:**

- `score < 0.10` — discarded, not stored
- `0.10 ≤ score < 0.65` — stored as a `pending` blame edge, no alert
- `score ≥ 0.65` — narrative generated, alert sent, status set to `resolved`

### Service inference from file paths

Changed files are mapped to billable cloud services using a pattern table. The mapping ships tuned for the built-in cost adapter's service names and lives in `internal/correlate/service_map.go` — extend it to match your infra layout and provider:

| File path pattern | Inferred service |
|---|---|
| `terraform/lambda/`, `lambda/` | serverless functions |
| `terraform/ecs/`, `Dockerfile` | container compute |
| `terraform/rds/`, `terraform/aurora/` | managed relational DB |
| `terraform/s3/`, `s3_*` | object storage |
| `terraform/dynamodb/`, `dynamo*` | NoSQL DB |
| `k8s/`, `kubernetes/`, `helm/` | managed Kubernetes |
| `terraform/ec2/`, `terraform/alb/` | VMs / load balancing |

---

## Anatomy of a Blame

A real run, end to end. (This example uses the default adapter stack — swap any layer and the flow is identical.)

```
1. PR #847 by alice merges to main
         │
         ▼
2. CI pipeline completes → webhook hits POST /webhooks/github
         │
   ┌─────┴───────────────────────────────────────┐
   │  costblame validates the HMAC signature     │
   │  → HTTP 202 Accepted (immediately)          │
   │  → background enrichment: PR title, author, │
   │    labels, changed file paths               │
   │  → DeployEvent stored                       │
   └─────────────────────────────────────────────┘
         │
3. (Next morning) the billing API shows
   serverless compute: $380 today vs $155 yesterday (+145%)
         │
         ▼
4. costblame's next poll catches it
   → z-score = 3.8 (> 2.0 threshold)
   → CostSnapshot saved with is_anomaly = true
         │
         ▼
5. Correlation engine picks up the unblamed anomaly
   → queries deploys in [spike - 72h, spike]
   → finds PR #847 (alice, 6h before the spike)
         │
         ▼
6. The scorer shows its work:
   temporal_proximity : 0.85  (6h window)   × 0.40 = 0.34
   service_match      : 1.00  (exact match) × 0.30 = 0.30
   tag_match          : 0.50  (neutral)     × 0.20 = 0.10
   historical_pattern : 0.50  (2 prior)     × 0.10 = 0.05
                                              ─────────
                                    TOTAL:      0.79  ✓
         │
         ▼
7. Score 0.79 ≥ 0.65 → high confidence
   → narrative generated:
     "Cost for serverless compute increased 145% (USD 155 → USD 380)
      on Jun 10. PR #847 (feat: enable provisioned concurrency) by
      @alice, deployed 6 hours before the spike, is the most likely
      cause. Review the concurrency settings on the payment-processor
      function and consider reverting if the increase is unintended."
         │
         ▼
8. Alert lands in the team channel. BlameEdge → "resolved".
   Total human effort so far: zero.
```

---

## Use Cases

**FinOps & cost control** — Close the loop between engineering actions and billing outcomes. Every cost spike gets a named owner, so conversations with teams start from evidence instead of guesswork.

**Platform engineering** — You own the cloud account; when costs spike, everyone asks you what happened. Answer immediately: *"It was PR #847 by Alice — she enabled provisioned concurrency on the payment function at 14:32."*

**Incident response** — Cost spikes are often symptoms: a runaway retry loop, a misconfigured autoscaler, a query plan regression. `costblame` shortens the path from *"the bill is high"* to *"we know why."*

**Cost-aware engineering culture** — When deploys are tracked against cost impact, engineers start thinking about cost at code review time. Cost becomes a first-class metric alongside latency and error rate.

**Pipeline automation** — Build cost gates into CI/CD: query the REST API after each deploy, post the result as a PR comment, or roll back automatically when a deploy blows past a threshold.

---

## Quickstart

### Prerequisites

- Docker + Docker Compose (or Go 1.22+ and `gcc` for a local build — CGO is required for SQLite)
- Read access to your cloud's cost API (see [Adapter setup](#adapter-setup))
- A URL your CI/CD system can reach for webhooks (ngrok works for local dev)

### Run with Docker Compose

```bash
# 1. Clone and configure
git clone https://github.com/prem0x01/costblame
cd costblame
cp .env.example .env
cp costblame.example.yaml costblame.yaml
$EDITOR .env           # credentials for the adapters you use
$EDITOR costblame.yaml # optional — env vars alone are enough

# 2. Start (migrations run first, then the daemon)
docker compose up

# 3. Local dev with an ngrok tunnel (so your CI/CD can reach you)
docker compose --profile dev up
```

The init container runs `costblame migrate` to apply the schema, exits 0, and then the main container starts. If migration fails, the main service never starts.

### Run locally

```bash
go install github.com/prem0x01/costblame/cmd/costblame@latest
cp costblame.example.yaml costblame.yaml && $EDITOR costblame.yaml
costblame migrate   # apply schema
costblame serve     # start the daemon
costblame tui       # terminal UI (separate terminal)
costblame report    # print the blame table to stdout
```

---

## CLI Reference

| Command | What it does |
|---|---|
| `costblame serve` | Run the full daemon: cost poller, webhook receivers, correlation engine, REST API |
| `costblame migrate` | Apply database migrations and exit — designed for init containers |
| `costblame report [-n 20]` | Print recent blame edges as a table (great in CI jobs and cron mails) |
| `costblame tui` | Open the interactive terminal UI |

Global flag: `-c, --config <file>` — config file path (default: `costblame.yaml`, also searched in `~/.costblame/` and `/etc/costblame/`).

**TUI keys:** `↑/↓` navigate · `enter` drill into an edge · `esc` back · `r` refresh · `q` quit.

---

## Configuration

Everything lives in `costblame.yaml` **or** environment variables — env wins, and env alone is sufficient. Each section selects a provider; adapter-specific settings sit in a matching sub-section, so swapping providers is a one-line change.

```yaml
cost:
  provider: aws                 # billing adapter — implement collect.CostSource to add more
  poll_interval: 15m
  lookback_days: 30             # baseline window for z-score
  anomaly_min_delta_pct: 20.0   # ignore spikes smaller than this
  aws:                          # settings for the selected provider
    region: us-east-1
    granularity: DAILY          # DAILY or HOURLY

database:
  driver: sqlite
  dsn: /data/costblame.db

sources:                        # CI/CD adapters — filled in = enabled
  github:
    webhook_secret: "your-secret"
    token: ""                   # optional — enables PR enrichment
  gitlab:
    webhook_secret: ""          # set to enable /webhooks/gitlab
  argocd:
    enabled: false              # explicit opt-in (ArgoCD webhooks have no secret)

llm:                            # optional — template narratives without it
  provider: ""                  # anthropic | openai | ollama | none | "" (auto-detect)
  api_key: ""                   # or the provider's conventional env var
  model: ""                     # empty = provider default
  base_url: ""                  # any OpenAI-compatible endpoint, hosted or local

notify:                         # every configured destination gets the alert
  slack:
    bot_token: "xoxb-..."
    channel: "C0EXAMPLE"
    min_confidence: 0.65
  webhook:                      # escape hatch for any other tool
    url: "https://example.com/hooks/costblame"
    min_confidence: 0.65

correlation:
  lookback_window: 72h
  high_confidence_threshold: 0.65
  min_score_to_store: 0.10
```

See [`costblame.example.yaml`](costblame.example.yaml) for the fully commented version.

### Environment variable reference

Any YAML key maps to `COSTBLAME_<SECTION>_<KEY>` (dots become underscores): `COSTBLAME_COST_PROVIDER`, `COSTBLAME_LLM_MODEL`, `COSTBLAME_NOTIFY_WEBHOOK_URL`, `COSTBLAME_DATABASE_DSN`, and so on. On top of that, costblame honors the conventional variables each ecosystem already uses:

| Variable | Effect |
|---|---|
| `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` / role | Credentials for the AWS billing adapter (standard SDK chain) |
| `ANTHROPIC_API_KEY` | Narratives via the Anthropic API |
| `OPENAI_API_KEY`, `OPENAI_BASE_URL` | Narratives via OpenAI or any compatible host |
| `OLLAMA_HOST` | Narratives via a local Ollama server |
| `GITHUB_WEBHOOK_SECRET`, `GITHUB_TOKEN` | Enables the GitHub source (+ PR enrichment) |
| `GITLAB_WEBHOOK_SECRET` | Enables the GitLab source |
| `SLACK_BOT_TOKEN`, `SLACK_CHANNEL` | Enables Slack alerts |

Explicit config (YAML or `COSTBLAME_*`) always wins over conventional fallbacks.

---

## REST API

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/blame` | List recent blame edges (resolved + confirmed) |
| `GET` | `/blame/:id` | Get a single blame edge with full context |
| `POST` | `/blame/:id/confirm` | Mark edge as confirmed (boosts historical scoring) |
| `POST` | `/blame/:id/dismiss` | Mark edge as dismissed (false positive) |
| `GET` | `/anomalies` | List unblamed cost anomalies |
| `GET` | `/healthz` | Health check — returns `ok` |

Webhook receivers are mounted on the same port at `POST /webhooks/<source>` for every enabled source.

---

## Adapter Setup

Setup notes for the adapters that ship in the box. Using something else? See [Extending costblame](#extending-costblame).

### Billing: AWS Cost Explorer

Minimum IAM policy:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["ce:GetCostAndUsage"],
      "Resource": "*"
    }
  ]
}
```

Credentials come from the standard SDK chain (env vars, shared config, IAM role / instance profile — prefer roles in production).

### Deploy events: GitHub Actions

1. Repo → **Settings → Webhooks → Add webhook**
2. Payload URL: `https://your-host:7890/webhooks/github`
3. Content type: `application/json`
4. Secret: the value of `sources.github.webhook_secret` (or `GITHUB_WEBHOOK_SECRET`)
5. Events: select **Workflow runs**

Set `sources.github.token` (or `GITHUB_TOKEN`) to enable PR metadata and changed-file enrichment.

### Deploy events: GitLab CI

1. Project → **Settings → Webhooks**
2. URL: `https://your-host:7890/webhooks/gitlab`
3. Secret token: the value of `sources.gitlab.webhook_secret` (or `GITLAB_WEBHOOK_SECRET`)
4. Trigger: **Pipeline events**

Successful pipelines on `main`/`master`/`release/*`/`deploy/*` branches are treated as deploys.

### Deploy events: ArgoCD

Set `sources.argocd.enabled: true` and point an ArgoCD notification webhook at `https://your-host:7890/webhooks/argocd`. ArgoCD webhooks carry no shared secret, so protect the endpoint at the network level.

### Narratives: any LLM, or none

Leave `llm.provider` empty and costblame auto-detects from your environment:

| You set | costblame uses |
|---|---|
| `ANTHROPIC_API_KEY` | the Anthropic API |
| `OPENAI_API_KEY` (and optionally `OPENAI_BASE_URL`) | OpenAI, or any compatible host |
| `OLLAMA_HOST` | your local Ollama server |
| `llm.base_url` | any OpenAI-compatible endpoint — Groq, Mistral, OpenRouter, vLLM, LM Studio, … |
| nothing | clean template narratives — no LLM calls, ever |

Pin a provider explicitly with `llm.provider` and override the model with `llm.model` (or `COSTBLAME_LLM_MODEL`).

### Alerts: Slack / anything

Slack uses a bot token + channel ID. For every other tool — incident management, ticketing, a custom dashboard — point `notify.webhook.url` at any HTTPS endpoint and you'll receive:

```json
{
  "edge_id": "…",
  "service": "…",
  "delta_pct": 145.0,
  "amount_usd": 380.0,
  "pr_number": 847,
  "pr_author": "alice",
  "confidence_score": 0.79,
  "narrative": "…",
  "detected_at": "2026-06-10T14:32:00Z"
}
```

---

## Extending costblame

Each integration slot is one small interface. Implement it, wire it up in `cmd/costblame/main.go`, and the engine treats it exactly like a built-in.

```go
// A billing source — any cloud, any FinOps platform.
type CostSource interface {
    Name() string
    Collect(ctx context.Context, from, to time.Time) ([]models.CostSnapshot, error)
}

// A deploy event source — webhook receiver or API poller.
type DeploySource interface {
    Name() string
    Events(ctx context.Context) (<-chan models.DeployEvent, error)
}

// A narrative generator — any LLM, or none.
type NarrativeGenerator interface {
    Generate(ctx context.Context, edge models.BlameEdge) (string, error)
}

// An alert destination — anything that can receive a message.
type Notifier interface {
    Send(ctx context.Context, graph models.BlameGraph) error
}
```

The existing adapters are the reference implementations: `internal/collect/aws` (~200 lines), `internal/notify/slack`, `internal/narrative`. Most new adapters are an afternoon of work. **Cost adapters for more clouds are the most wanted contribution.**

---

## FAQ

**Does my billing data leave my infrastructure?**
Cost data, deploy history, and the blame graph live in a local SQLite file. The only optional egress is the narrative prompt — anomaly and deploy metadata sent to *your chosen* LLM provider. Use Ollama (or `provider: none`) and even that stays local.

**Do I need an LLM?**
No. Without one, costblame generates clean template narratives. Everything else — detection, scoring, alerting, the API, the TUI — is deterministic Go code.

**What if it blames the wrong deploy?**
Dismiss it — one API call or one keypress in the TUI. Every blame shows its four factor scores, so you can see exactly why it was made, and dismissals are remembered. Blame is evidence-ranked *probability*, not a verdict; the human always has the last word.

**Is this only for AWS / GitHub / Slack?**
No — those are just the adapters that ship today. Deploy sources already cover GitHub Actions, GitLab CI, and ArgoCD; narratives cover Anthropic, OpenAI, Ollama, and any OpenAI-compatible endpoint; alerts cover Slack and any webhook. Billing currently ships with AWS, and the `CostSource` interface is ready for more clouds.

**What does it cost to run?**
A single small Go process and a SQLite file — it runs comfortably on the tiniest VM you have. If you enable a hosted LLM, you pay for 2–3 sentences per high-confidence anomaly; with templates or a local model, narrative cost is zero.

**Is it a SaaS? Is there a paid tier?**
No and no. MIT-licensed, self-hosted, yours.

---

## Components

- **`internal/collect`** — the `CostSource` and `DeploySource` interfaces the engine depends on. Cloud- and CI-agnostic.
- **`internal/collect/aws`** — billing adapter for AWS Cost Explorer: polls cost data, computes the rolling baseline, flags z-score anomalies.
- **`internal/collect/github`** — deploy adapter for GitHub Actions: validates HMAC-SHA256 webhook signatures, acks immediately (HTTP 202), enriches PR metadata asynchronously.
- **`internal/collect/gitlab` / `internal/collect/argocd`** — deploy adapters for GitLab CI pipeline events and ArgoCD sync events, following the same two-phase pattern.
- **`internal/correlate/engine`** — the main loop: finds unblamed anomalies, scores deploy candidates, persists edges, triggers narratives and alerts.
- **`internal/correlate/scorer`** — stateless, fully testable scoring logic. The `HistoryReader` interface is injected, so the scorer never touches the store directly.
- **`internal/narrative`** — the `NarrativeGenerator` adapters: Anthropic, OpenAI-compatible (hosted or local), and a template fallback.
- **`internal/store/sqlite`** — embedded storage, WAL mode. Three tables: `cost_snapshots`, `deploy_events`, `blame_edges`.
- **`internal/api`** — stdlib HTTP server (Go 1.22 pattern routing).
- **`internal/tui`** — interactive terminal UI: blame timeline with drill-down.
- **`internal/notify`** — the `Notifier` interface with fan-out, plus Slack and generic webhook adapters.

---

## Testing

```bash
# Scenario tests against a live instance
WEBHOOK_SECRET=changeme ./scripts/test-scenarios.sh

# Individual scenarios
./scripts/test-scenarios.sh health    # health check
./scripts/test-scenarios.sh webhook   # simulate a deploy event
./scripts/test-scenarios.sh bad_sig   # verify HMAC rejection
./scripts/test-scenarios.sh blame     # query blame edges

# Unit + integration tests
CGO_ENABLED=1 go test -race ./...
```

---

## Technology Stack

| Layer | Technology |
|-------|-----------|
| Language | Go 1.22 |
| Storage | SQLite (embedded, WAL mode) |
| CLI | Cobra + Viper |
| Terminal UI | Bubble Tea + Bubbles + Lipgloss |
| HTTP | Go stdlib `net/http` (pattern routing) |
| Billing / CI / LLM / alerts | Pluggable adapters — see [Bring Your Own Everything](#bring-your-own-everything) |
| Packaging | Docker multi-stage build, Docker Compose |

---

## License

MIT

---

<div align="center">

**Stop guessing. Start blaming (kindly).**

If costblame saved you an investigation, ⭐ the repo — and if your cloud, CI, or LLM isn't in the adapter list yet, [an adapter is an afternoon of work](#extending-costblame). PRs welcome.

</div>

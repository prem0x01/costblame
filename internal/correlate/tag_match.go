package correlate

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/prem0x01/costblame/pkg/models"
)

// Tag matching compares the allocation tags attributed to an anomaly (team, env)
// with what a deploy source knows about the deploy. The two sides rarely spell
// things alike: AWS tags say "Production" or "prd", GitHub reports "prod" for a
// workflow and "production" for a deployment, GitLab says "production", and
// ArgoCD reports a Kubernetes namespace. Comparing raw strings would turn those
// spelling differences into false mismatches that cost a correct alert its
// score, so each side is reduced to a comparable identity first, and a check
// that cannot be made is skipped rather than failed.

// Environment classes.
const (
	EnvProd    = "prod"
	EnvStaging = "staging"
	EnvDev     = "dev"
	EnvTest    = "test"
)

var envWords = map[string]string{
	"staging": EnvStaging, "stage": EnvStaging, "stg": EnvStaging, "preprod": EnvStaging,
	"nonprod": EnvStaging, "uat": EnvStaging, "preview": EnvStaging, "pre": EnvStaging, "non": EnvStaging,
	"dev": EnvDev, "development": EnvDev, "develop": EnvDev, "sandbox": EnvDev, "local": EnvDev,
	"test": EnvTest, "testing": EnvTest, "qa": EnvTest,
	"prod": EnvProd, "production": EnvProd, "prd": EnvProd, "live": EnvProd,
}

// EnvClass reduces an environment name to prod, staging, dev or test, or "" when
// it cannot tell ("unknown", an empty value, a namespace such as "payments").
// Names are read word by word, so "prod-eu" and "payments-prod" are prod while
// "preprod", "pre-production" and "non-prod" are staging. Non-production words
// win over production ones: "staging-production-like" is not production.
func EnvClass(name string) string {
	words := strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	class := ""
	for _, w := range words {
		c, ok := envWords[w]
		if !ok {
			if rest, found := strings.CutPrefix(w, "prod"); found && isDigits(rest) {
				c, ok = EnvProd, true // "prod1", "prod2"
			}
		}
		switch {
		case !ok:
		case c == EnvProd:
			if class == "" {
				class = EnvProd
			}
		default:
			return c // any non-production word decides it
		}
	}
	return class
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// serviceSuffixes are stripped from a repository name to get the team's name:
// "payments-api" -> "payments".
var serviceSuffixes = []string{"-api", "-service", "-worker", "-backend", "-server"}

// deployTeamNames lists the names a deploy's team could go by: its team label,
// if any, and the repository's name (last path segment, however the repository
// is written: owner/repo, a nested GitLab path or an ArgoCD URL) with a service
// suffix removed, plus each word of it.
func deployTeamNames(d models.DeployEvent) []string {
	var names []string
	if d.PRTeam != "" {
		names = append(names, d.PRTeam)
	}
	path := RepoPath(d.Repository)
	if i := strings.LastIndex(path, "/"); i >= 0 {
		path = path[i+1:]
	}
	base := strings.ToLower(strings.TrimSpace(path))
	if base == "" {
		return names
	}
	for _, suffix := range serviceSuffixes {
		base = strings.TrimSuffix(base, suffix)
	}
	names = append(names, base)
	for _, w := range strings.FieldsFunc(base, func(r rune) bool { return r == '-' || r == '_' || r == '.' }) {
		if len(w) >= 3 {
			names = append(names, w)
		}
	}
	return names
}

// teamMatches reports whether the team tag names the deploy's team.
func teamMatches(team string, d models.DeployEvent) bool {
	want := squash(team)
	if want == "" {
		return false
	}
	for _, name := range deployTeamNames(d) {
		if squash(name) == want {
			return true
		}
	}
	return false
}

// tagEvidence compares the anomaly's team and env tags with the deploy and
// returns how many comparisons matched, how many were made, and a reason.
//
// Environment is reliable evidence either way: a staging deploy blamed for
// production spend is a real mismatch. It is only compared when both sides name
// a recognisable environment. Team is corroboration only: mapping a repository
// name to a team is a heuristic (teams do not always name their repositories
// after themselves), so a team that does not match is not held against the
// deploy; a team that matches counts in its favour.
func tagEvidence(tags map[string]string, d models.DeployEvent) (matches, checks int, reason string) {
	var parts []string
	if env := tags["env"]; env != "" {
		a, b := EnvClass(env), EnvClass(d.Environment)
		if a != "" && b != "" {
			checks++
			if a == b {
				matches++
				parts = append(parts, fmt.Sprintf("env %s = %s", env, d.Environment))
			} else {
				parts = append(parts, fmt.Sprintf("env %s != %s", env, d.Environment))
			}
		}
	}
	if team := tags["team"]; team != "" && teamMatches(team, d) {
		checks++
		matches++
		parts = append(parts, fmt.Sprintf("team %s matches %s", team, d.Repository))
	}
	return matches, checks, strings.Join(parts, "; ")
}

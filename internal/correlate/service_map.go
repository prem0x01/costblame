package correlate

import (
	"fmt"
	"strings"
)

// servicePattern maps a file path substring to an AWS service name.
type servicePattern struct {
	pattern string
	service string
}

// defaultPatterns covers the most common IaC and application layouts.
// Teams can add their own with correlation.path_patterns (checked first); see ServiceMap.
var defaultPatterns = []servicePattern{
	{"terraform/ecs", "AmazonECS"},
	{"terraform/rds", "AmazonRDS"},
	{"terraform/aurora", "AmazonRDS"},
	{"terraform/lambda", "AWSLambda"},
	{"terraform/s3", "AmazonS3"},
	{"terraform/elasticache", "AmazonElastiCache"},
	{"terraform/cloudfront", "AmazonCloudFront"},
	{"terraform/sqs", "AWSQueueService"},
	{"terraform/sns", "AmazonSNS"},
	{"terraform/eks", "AmazonEKS"},
	{"terraform/ec2", "AmazonEC2"},
	{"terraform/alb", "AmazonElasticLoadBalancing"}, // load balancers are their own Cost Explorer service
	{"terraform/nlb", "AmazonElasticLoadBalancing"},
	{"terraform/dynamodb", "AmazonDynamoDB"},
	{"terraform/kinesis", "AmazonKinesis"},
	{"terraform/opensearch", "AmazonOpenSearchService"},
	{"k8s/", "AmazonEKS"},
	{"kubernetes/", "AmazonEKS"},
	{"helm/", "AmazonEKS"},
	{"Dockerfile", "AmazonECS"},
	{"docker-compose", "AmazonECS"},
	// Generic path-based signals — lower precedence than IaC-specific patterns above.
	{"lambda", "AWSLambda"},
	{"dynamo", "AmazonDynamoDB"},
	{"kinesis", "AmazonKinesis"},
	{"sagemaker", "AmazonSageMaker"},
	{"bedrock", "AmazonBedrock"},
	{"s3_", "AmazonS3"},
	{"_s3", "AmazonS3"},
	{"/s3/", "AmazonS3"},
	{"/s3.", "AmazonS3"},
}

// InferServicesFromFiles maps a list of changed file paths to AWS service names
// using the default pattern table. Results are deduplicated.
// This is package-level (not a method) so callers without a configured
// ServiceMap can use the defaults directly.
func InferServicesFromFiles(files []string) []string {
	return (*ServiceMap)(nil).InferFromFiles(files)
}

// ServiceMapEntry assigns cloud services to every deploy from a repository or
// application, whatever files it changed. It is how deploys from sources that
// cannot list changed files (ArgoCD, or GitHub/GitLab without an API token) can
// still match a spiking service.
type ServiceMapEntry struct {
	// Match is a glob over a repository path ("acme/payments-api"), an
	// ArgoCD application name, or a repository URL. "*" matches any run of
	// characters, "/" included; matching ignores case.
	Match string
	// Services are the services those deploys affect. Product codes
	// ("AWSLambda"), display names ("AWS Lambda") and short names ("lambda")
	// all work; see CanonicalService.
	Services []string
}

// PathPattern adds a file-path rule, checked before the built-in patterns:
// a changed file whose path contains Pattern (ignoring case) affects Service.
type PathPattern struct {
	Pattern string
	Service string
}

// ServiceMap holds the user's service ownership rules. The zero value and nil
// are valid and mean "built-in patterns only".
type ServiceMap struct {
	entries []ServiceMapEntry
	paths   []servicePattern
}

// NewServiceMap validates and builds a ServiceMap. Rules that could never match
// anything useful are rejected so a typo fails at startup, not silently.
func NewServiceMap(entries []ServiceMapEntry, paths []PathPattern) (*ServiceMap, error) {
	m := &ServiceMap{}
	for i, e := range entries {
		if strings.TrimSpace(e.Match) == "" {
			return nil, fmt.Errorf("service_map[%d]: match must not be empty", i)
		}
		var services []string
		for _, svc := range e.Services {
			if CanonicalService(svc) == "" {
				return nil, fmt.Errorf("service_map[%d] (%q): blank service name %q", i, e.Match, svc)
			}
			services = append(services, strings.TrimSpace(svc))
		}
		if len(services) == 0 {
			return nil, fmt.Errorf("service_map[%d] (%q): services must not be empty", i, e.Match)
		}
		m.entries = append(m.entries, ServiceMapEntry{Match: strings.ToLower(strings.TrimSpace(e.Match)), Services: services})
	}
	for i, p := range paths {
		if strings.TrimSpace(p.Pattern) == "" || CanonicalService(p.Service) == "" {
			return nil, fmt.Errorf("path_patterns[%d]: both pattern and service are required", i)
		}
		m.paths = append(m.paths, servicePattern{strings.TrimSpace(p.Pattern), strings.TrimSpace(p.Service)})
	}
	return m, nil
}

// HasRepoRules reports whether any repository/application rule is configured.
func (m *ServiceMap) HasRepoRules() bool { return m != nil && len(m.entries) > 0 }

// ForKeys returns the services of every rule matching any of keys (a
// repository path, an application name, a repository URL). Nil-safe.
func (m *ServiceMap) ForKeys(keys ...string) []string {
	if m == nil {
		return nil
	}
	var out []string
	for _, e := range m.entries {
		for _, k := range keys {
			if k != "" && globMatch(e.Match, strings.ToLower(k)) {
				out = append(out, e.Services...)
				break
			}
		}
	}
	return UnionServices(out)
}

// InferFromFiles maps changed file paths to services: the user's path patterns
// first, then the built-in table. One service per file at most; results are
// deduplicated. Nil-safe.
func (m *ServiceMap) InferFromFiles(files []string) []string {
	var custom []servicePattern
	if m != nil {
		custom = m.paths
	}
	var services []string
	for _, f := range files {
		lower := strings.ToLower(f)
	next:
		for _, table := range [][]servicePattern{custom, defaultPatterns} {
			for _, p := range table {
				if p.service != "" && strings.Contains(lower, strings.ToLower(p.pattern)) {
					services = append(services, p.service)
					break next
				}
			}
		}
	}
	return UnionServices(services)
}

// UnionServices merges service lists, dropping duplicates by identity
// ("lambda" and "AWS Lambda" are one service) and keeping the first spelling
// and the original order.
func UnionServices(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range lists {
		for _, svc := range list {
			id := CanonicalService(svc)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, svc)
		}
	}
	return out
}

// RepoPath reduces a repository URL to "owner/repo", e.g.
// "https://github.com/acme/infra.git" and "git@github.com:acme/infra.git" both
// become "acme/infra", so one service-map rule matches however ArgoCD spells it.
// A value that is not a URL is returned trimmed.
func RepoPath(repoURL string) string {
	s := strings.TrimSpace(repoURL)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
		if j := strings.Index(s, "/"); j >= 0 {
			s = s[j+1:]
		} else {
			return ""
		}
	} else if i := strings.Index(s, "@"); i >= 0 {
		if j := strings.Index(s[i:], ":"); j >= 0 { // scp-style git@host:owner/repo
			s = s[i+j+1:]
		}
	}
	s = strings.TrimSuffix(strings.Trim(s, "/"), ".git")
	return strings.Trim(s, "/")
}

// globMatch reports whether s matches pattern, where "*" matches any run of
// characters (including "/") and "?" matches exactly one. Both are expected to
// be lower-case already.
func globMatch(pattern, s string) bool {
	p, n := []rune(pattern), []rune(s)
	pi, si := 0, 0
	star, mark := -1, 0
	for si < len(n) {
		switch {
		case pi < len(p) && (p[pi] == '?' || p[pi] == n[si]):
			pi++
			si++
		case pi < len(p) && p[pi] == '*':
			star, mark = pi, si
			pi++
		case star >= 0:
			pi = star + 1
			mark++
			si = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}

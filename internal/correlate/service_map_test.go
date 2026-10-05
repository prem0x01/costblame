package correlate

import (
	"testing"
)

func TestInferServicesFromFiles_Lambda(t *testing.T) {
	files := []string{
		"services/payment/handler.go",
		"lambda/processor/main.go",
		"internal/util/helpers.go",
	}
	got := InferServicesFromFiles(files)
	if !contains(got, "AWSLambda") {
		t.Errorf("expected AWSLambda in %v", got)
	}
}

func TestInferServicesFromFiles_S3(t *testing.T) {
	files := []string{"storage/s3_client.go", "cmd/upload/main.go"}
	got := InferServicesFromFiles(files)
	if !contains(got, "AmazonS3") {
		t.Errorf("expected AmazonS3 in %v", got)
	}
}

func TestInferServicesFromFiles_DynamoDB(t *testing.T) {
	files := []string{"database/dynamo.go"}
	got := InferServicesFromFiles(files)
	if !contains(got, "AmazonDynamoDB") {
		t.Errorf("expected AmazonDynamoDB in %v", got)
	}
}

func TestInferServicesFromFiles_NoMatch(t *testing.T) {
	files := []string{"docs/README.md", "CHANGELOG.md"}
	got := InferServicesFromFiles(files)
	if len(got) != 0 {
		t.Errorf("expected no services for doc-only files, got %v", got)
	}
}

func TestInferServicesFromFiles_Dedup(t *testing.T) {
	files := []string{
		"lambda/func1/main.go",
		"lambda/func2/main.go",
		"lambda/func3/main.go",
	}
	got := InferServicesFromFiles(files)
	count := 0
	for _, s := range got {
		if s == "AWSLambda" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected AWSLambda exactly once, got %d times in %v", count, got)
	}
}

func TestInferServicesFromFiles_Empty(t *testing.T) {
	got := InferServicesFromFiles(nil)
	if len(got) != 0 {
		t.Errorf("expected empty slice for nil input, got %v", got)
	}
}

func contains(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

func mustMap(t *testing.T, entries []ServiceMapEntry, paths []PathPattern) *ServiceMap {
	t.Helper()
	m, err := NewServiceMap(entries, paths)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestServiceMap_ForKeys(t *testing.T) {
	m := mustMap(t, []ServiceMapEntry{
		{Match: "acme/payments-api", Services: []string{"AWS Lambda", "DynamoDB"}},
		{Match: "acme/data-*", Services: []string{"s3"}},
		{Match: "payments", Services: []string{"lambda", "RDS"}}, // an ArgoCD app name; overlaps on Lambda
		{Match: "*/infra", Services: []string{"EKS"}},
	}, nil)

	cases := []struct {
		keys []string
		want []string
	}{
		{[]string{"acme/payments-api"}, []string{"AWS Lambda", "DynamoDB"}},
		{[]string{"ACME/Payments-API"}, []string{"AWS Lambda", "DynamoDB"}}, // case-insensitive
		{[]string{"acme/data-pipeline"}, []string{"s3"}},                    // glob
		{[]string{"acme/data-"}, []string{"s3"}},                            // * matches the empty run
		{[]string{"org/sub/infra"}, []string{"EKS"}},                        // * crosses "/"
		{[]string{"acme/unrelated"}, nil},                                   // no rule
		{[]string{"", "acme/unrelated"}, nil},                               // blank keys are ignored
		{nil, nil},
	}
	for _, tc := range cases {
		got := m.ForKeys(tc.keys...)
		if len(got) != len(tc.want) {
			t.Errorf("ForKeys(%v) = %v, want %v", tc.keys, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("ForKeys(%v) = %v, want %v", tc.keys, got, tc.want)
			}
		}
	}

	// Services from several matching rules are unioned; "lambda" and "AWS Lambda" are one service.
	got := m.ForKeys("payments", "acme/payments-api")
	if len(got) != 3 { // lambda (first spelling), RDS, DynamoDB
		t.Errorf("overlapping rules should union by service identity, got %v", got)
	}
}

func TestServiceMap_NilIsSafe(t *testing.T) {
	var m *ServiceMap
	if m.HasRepoRules() {
		t.Error("nil map reports rules")
	}
	if got := m.ForKeys("acme/x"); got != nil {
		t.Errorf("nil ForKeys = %v", got)
	}
	if got := m.InferFromFiles([]string{"terraform/lambda/main.tf"}); len(got) != 1 || got[0] != "AWSLambda" {
		t.Errorf("a nil map should fall back to the built-in patterns, got %v", got)
	}
}

func TestServiceMap_CustomPathPatternsComeFirst(t *testing.T) {
	m := mustMap(t, nil, []PathPattern{
		{Pattern: "services/billing/", Service: "AmazonDynamoDB"},
		{Pattern: "terraform/lambda", Service: "Amazon SQS"}, // overrides the built-in meaning
	})
	got := m.InferFromFiles([]string{
		"services/billing/handler.go", // custom
		"terraform/lambda/main.tf",    // custom beats built-in
		"terraform/s3/bucket.tf",      // falls through to the built-in table
		"README.md",                   // nothing
	})
	want := []string{"AmazonDynamoDB", "Amazon SQS", "AmazonS3"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %v, want %v", got, want)
		}
	}
}

func TestNewServiceMap_RejectsUselessRules(t *testing.T) {
	cases := map[string]struct {
		entries []ServiceMapEntry
		paths   []PathPattern
	}{
		"empty match":     {entries: []ServiceMapEntry{{Match: " ", Services: []string{"lambda"}}}},
		"no services":     {entries: []ServiceMapEntry{{Match: "acme/x"}}},
		"blank service":   {entries: []ServiceMapEntry{{Match: "acme/x", Services: []string{"lambda", " - "}}}},
		"path no pattern": {paths: []PathPattern{{Service: "lambda"}}},
		"path no service": {paths: []PathPattern{{Pattern: "lambda/"}}},
	}
	for name, tc := range cases {
		if _, err := NewServiceMap(tc.entries, tc.paths); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, err := NewServiceMap(nil, nil); err != nil {
		t.Errorf("an empty map is valid: %v", err)
	}
}

func TestRepoPath(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/acme/infra.git":        "acme/infra",
		"https://github.com/acme/infra":            "acme/infra",
		"https://github.com/acme/infra/":           "acme/infra",
		"git@github.com:acme/infra.git":            "acme/infra",
		"ssh://git@gitlab.example.com/grp/sub/app": "grp/sub/app",
		"https://gitlab.example.com/grp/sub/app":   "grp/sub/app",
		"acme/infra":                               "acme/infra",
		"payments":                                 "payments",
		"https://github.com":                       "",
		"":                                         "",
	} {
		if got := RepoPath(in); got != want {
			t.Errorf("RepoPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUnionServices_DedupesByIdentity(t *testing.T) {
	got := UnionServices([]string{"AWS Lambda", "s3"}, []string{"lambda", "AmazonS3", "RDS"}, nil, []string{"", "rds"})
	want := []string{"AWS Lambda", "s3", "RDS"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %v, want %v", got, want)
		}
	}
}

func TestGlobMatch(t *testing.T) {
	for _, tc := range []struct {
		pattern, s string
		want       bool
	}{
		{"acme/*", "acme/payments", true}, {"acme/*", "acme/a/b", true}, {"acme/*", "other/x", false},
		{"*", "", true}, {"*", "anything/at/all", true}, {"a?c", "abc", true}, {"a?c", "ac", false},
		{"*-api", "payments-api", true}, {"*-api", "payments-apix", false}, {"a*b*c", "aXXbYYc", true},
		{"", "", true}, {"", "x", false}, {"x", "", false},
	} {
		if got := globMatch(tc.pattern, tc.s); got != tc.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tc.pattern, tc.s, got, tc.want)
		}
	}
}

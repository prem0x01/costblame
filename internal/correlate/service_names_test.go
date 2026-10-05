package correlate

import (
	"context"
	"testing"
	"time"

	"github.com/prem0x01/costblame/pkg/models"
)

// Names exactly as Cost Explorer's SERVICE dimension reports them, paired with
// what the service map infers from a deploy's files. Before CanonicalService
// every pair scored 0 and no alert could ever fire for real AWS data.
var costExplorerNames = []struct{ ce, inferred string }{
	{"AWS Lambda", "AWSLambda"},
	{"Amazon Elastic Compute Cloud - Compute", "AmazonEC2"},
	{"EC2 - Other", "AmazonEC2"},
	{"Amazon Simple Storage Service", "AmazonS3"},
	{"Amazon Relational Database Service", "AmazonRDS"},
	{"Amazon DynamoDB", "AmazonDynamoDB"},
	{"Amazon Elastic Container Service", "AmazonECS"},
	{"Amazon Elastic Kubernetes Service", "AmazonEKS"},
	{"Amazon Simple Queue Service", "AWSQueueService"},
	{"Amazon Simple Notification Service", "AmazonSNS"},
	{"Amazon ElastiCache", "AmazonElastiCache"},
	{"Amazon CloudFront", "AmazonCloudFront"},
	{"Amazon OpenSearch Service", "AmazonOpenSearchService"},
	{"Amazon Elasticsearch Service", "AmazonOpenSearchService"}, // pre-rename name
	{"Amazon Elastic Load Balancing", "AmazonElasticLoadBalancing"},
}

func serviceMatchScore(anomalyService string, inferred ...string) float64 {
	s := NewScorer(&fakeHistory{})
	return s.serviceScore(models.CostSnapshot{Service: anomalyService}, models.DeployEvent{InferredServices: inferred}).Score
}

func TestServiceMatch_RealCostExplorerNames(t *testing.T) {
	for _, c := range costExplorerNames {
		if got := serviceMatchScore(c.ce, c.inferred); got != 1.0 {
			t.Errorf("service_match(%q vs %q) = %.1f, want 1.0", c.ce, c.inferred, got)
		}
	}
}

func TestServiceMatch_AcceptsEveryWayToWriteAService(t *testing.T) {
	// A hand-written service map may use product codes, display names or short names.
	for _, spelling := range []string{"lambda", "Lambda", "AWSLambda", "AWS Lambda", "aws-lambda", "AWS_LAMBDA"} {
		if got := serviceMatchScore("AWS Lambda", spelling); got != 1.0 {
			t.Errorf("%q should match AWS Lambda exactly, got %.1f", spelling, got)
		}
	}
	for _, spelling := range []string{"s3", "S3", "AmazonS3", "Amazon Simple Storage Service"} {
		if got := serviceMatchScore("Amazon Simple Storage Service", spelling); got != 1.0 {
			t.Errorf("%q should match S3 exactly, got %.1f", spelling, got)
		}
	}
	for _, spelling := range []string{"alb", "nlb", "elb"} {
		if got := serviceMatchScore("Amazon Elastic Load Balancing", spelling); got != 1.0 {
			t.Errorf("%q should match Elastic Load Balancing, got %.1f", spelling, got)
		}
	}
}

func TestServiceMatch_DifferentServicesDoNotMatch(t *testing.T) {
	cases := []struct{ ce, inferred string }{
		{"AWS Lambda", "AmazonS3"},
		{"Amazon Simple Storage Service", "AmazonEC2"},
		{"Amazon Relational Database Service", "AmazonDynamoDB"},
		{"Amazon Elastic Container Service", "AmazonEKS"}, // ECS is not EKS
		{"Amazon Simple Queue Service", "AmazonSNS"},
		{"Amazon Elastic Compute Cloud - Compute", "AmazonElasticLoadBalancing"},
	}
	for _, c := range cases {
		if got := serviceMatchScore(c.ce, c.inferred); got != 0 {
			t.Errorf("service_match(%q vs %q) = %.1f, want 0", c.ce, c.inferred, got)
		}
	}
	if got := serviceMatchScore("AWS Lambda"); got != 0 {
		t.Errorf("no inferred services should score 0, got %.1f", got)
	}
	if got := serviceMatchScore("AWS Lambda", "", "  ", "--"); got != 0 {
		t.Errorf("blank or punctuation-only names must not match, got %.1f", got)
	}
}

// Squashing spaces makes "amazonec2" a substring of longer names that are
// different services. They must not earn a partial match from the shared prefix.
func TestServiceMatch_LongerNamesDoNotBorrowAShorterServicesCredit(t *testing.T) {
	cases := []struct {
		ce, inferred string
		want         float64
	}{
		{"Amazon EC2 Container Registry (ECR)", "AmazonEC2", 0}, // would have been a 0.7 partial: score 0.71, a false alert
		{"Amazon EC2 Container Registry (ECR)", "ecr", 1.0},
		{"Amazon Elastic Container Registry", "ecr", 1.0},
		{"Amazon EC2 Container Service", "AmazonECS", 1.0}, // the old name of ECS
		{"Amazon EC2 Container Service", "AmazonEC2", 0},
		{"Amazon Elastic Container Service", "AmazonEC2", 0},
		{"Amazon Elastic Kubernetes Service", "AmazonECS", 0},
		{"Amazon S3 Glacier", "AmazonS3", 0.7}, // unknown name: partial credit on purpose, Glacier is S3-family storage
	}
	for _, c := range cases {
		if got := serviceMatchScore(c.ce, c.inferred); got != c.want {
			t.Errorf("service_match(%q vs %q) = %.1f, want %.1f", c.ce, c.inferred, got, c.want)
		}
	}
}

func TestServiceMatch_PartialStillWorksForUnknownNames(t *testing.T) {
	// The anomaly's service name contains the inferred one: a partial match.
	if got := serviceMatchScore("Amazon Kinesis Firehose", "Kinesis"); got != 0.7 {
		t.Errorf("partial match = %.1f, want 0.7", got)
	}
	// Too short to be a meaningful token.
	if got := serviceMatchScore("Amazon Kinesis Firehose", "ki"); got != 0 {
		t.Errorf("a 2-character token must not partially match, got %.1f", got)
	}
}

func TestCanonicalService(t *testing.T) {
	same := [][]string{
		{"AWS Lambda", "AWSLambda", "lambda", " aws  lambda "},
		{"Amazon Elastic Compute Cloud - Compute", "AmazonEC2", "ec2", "EC2 - Other"},
		{"Amazon Simple Storage Service", "AmazonS3", "s3"},
	}
	for _, group := range same {
		want := CanonicalService(group[0])
		for _, name := range group[1:] {
			if got := CanonicalService(name); got != want {
				t.Errorf("CanonicalService(%q) = %q, want %q (same as %q)", name, got, want, group[0])
			}
		}
	}
	if CanonicalService("AWS Lambda") == CanonicalService("Amazon S3") {
		t.Error("different services share an identity")
	}
	if CanonicalService("") != "" || CanonicalService(" - ") != "" {
		t.Error("blank names should canonicalise to empty")
	}
}

func TestInferredServicesMatchCostExplorerNamesEndToEnd(t *testing.T) {
	// The path -> service map and the scorer must agree with Cost Explorer.
	for path, ce := range map[string]string{
		"terraform/lambda/main.tf":    "AWS Lambda",
		"terraform/s3/bucket.tf":      "Amazon Simple Storage Service",
		"terraform/rds/cluster.tf":    "Amazon Relational Database Service",
		"terraform/dynamodb/table.tf": "Amazon DynamoDB",
		"terraform/ecs/service.tf":    "Amazon Elastic Container Service",
		"terraform/alb/listener.tf":   "Amazon Elastic Load Balancing",
	} {
		inferred := InferServicesFromFiles([]string{path})
		if len(inferred) != 1 {
			t.Fatalf("%s inferred %v", path, inferred)
		}
		if got := serviceMatchScore(ce, inferred...); got != 1.0 {
			t.Errorf("%s -> %v does not match Cost Explorer's %q (score %.1f)", path, inferred, ce, got)
		}
	}
}

// The regression that mattered: a deploy that touches the right files used to
// score 0 on service_match against Cost Explorer's names, so the best possible
// score was 0.60 and nothing was ever alerted.
func TestRealCostExplorerSpikeWithMatchingDeployAlerts(t *testing.T) {
	env := newEnv(t, 0.2)
	anomaly := anomalyAt(spike)
	anomaly.Service = "AWS Lambda" // as Cost Explorer reports it
	env.seedAnomaly(anomaly)
	d := deployAt(spike.Add(-time.Hour))
	d.InferredServices = InferServicesFromFiles([]string{"terraform/lambda/function.tf"}) // [AWSLambda]
	env.seedDeploy(d)

	env.cycle()

	edges := env.edges(anomaly)
	if len(edges) != 1 {
		t.Fatalf("edges = %d, want 1", len(edges))
	}
	if edges[0].ConfidenceScore < HighConfidenceThreshold {
		t.Errorf("score = %.2f, below the %.2f alert threshold: service_match failed against a real Cost Explorer name",
			edges[0].ConfidenceScore, HighConfidenceThreshold)
	}
	if len(env.notifier.sent) != 1 {
		t.Errorf("alerts sent = %d, want 1", len(env.notifier.sent))
	}
	_ = context.Background
}

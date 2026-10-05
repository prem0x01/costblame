package correlate

import "strings"

// servicePattern maps a file path substring to an AWS service name.
type servicePattern struct {
	pattern string
	service string
}

// defaultPatterns covers the most common IaC and application layouts.
// Teams can extend this via the config file (see CorrelationConfig.ServiceMapFile).
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
	{"terraform/alb", "AmazonEC2"}, // ALB costs appear under EC2
	{"terraform/nlb", "AmazonEC2"},
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
// This is package-level (not a method) so the GitHub webhook and poller can
// call it without importing the full Engine.
func InferServicesFromFiles(files []string) []string {
	seen := map[string]bool{}
	var services []string
	for _, f := range files {
		lower := strings.ToLower(f)
		for _, p := range defaultPatterns {
			if p.service != "" && strings.Contains(lower, strings.ToLower(p.pattern)) {
				if !seen[p.service] {
					seen[p.service] = true
					services = append(services, p.service)
				}
				break
			}
		}
	}
	return services
}

// serviceContains reports whether the AWS service name contains the short token
// (case-insensitive). E.g. serviceContains("AmazonECS", "ecs") → true.
func serviceContains(awsService, token string) bool {
	return strings.Contains(strings.ToLower(awsService), strings.ToLower(token))
}

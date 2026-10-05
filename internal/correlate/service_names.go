package correlate

import (
	"strings"
	"unicode"
)

// A cloud service has several names. Cost Explorer's SERVICE dimension uses
// display names ("Amazon Elastic Compute Cloud - Compute", "AWS Lambda"), the
// service map and most people write product codes ("AmazonEC2", "AWSLambda") or
// short names ("ec2", "lambda"). Comparing them as raw strings never matched
// real billing data, so service_match, a third of every score, was always 0.
//
// CanonicalService reduces every spelling to one identity so they compare equal.

// serviceAliases maps a squashed name (lower case, letters and digits only) to
// the squashed product code of the same service. A name that is not here is its
// own identity, so only spellings that differ from the product code need an
// entry; display names that squash to the product code ("AWS Lambda" ->
// "awslambda") already agree.
var serviceAliases = map[string]string{
	// Cost Explorer display names.
	"amazonelasticcomputecloudcompute":           "amazonec2",
	"amazonelasticcomputecloud":                  "amazonec2",
	"ec2other":                                   "amazonec2",
	"amazonsimplestorageservice":                 "amazons3",
	"amazonrelationaldatabaseservice":            "amazonrds",
	"amazonelasticcontainerservice":              "amazonecs",
	"amazonelastickubernetesservice":             "amazoneks",
	"amazonelasticcontainerserviceforkubernetes": "amazoneks",
	"amazonsimplequeueservice":                   "awsqueueservice",
	"amazonsimplenotificationservice":            "amazonsns",
	"amazonelasticsearchservice":                 "amazonopensearchservice",
	"awsqueueservice":                            "awsqueueservice",

	// Names that merely START with another service's name once squashed. Left
	// alone, "Amazon EC2 Container Registry (ECR)" would partially match
	// AmazonEC2 and a deploy touching terraform/ec2/ would alert on an ECR spike.
	"amazonec2containerregistryecr":  "amazonecr",
	"amazonec2containerregistry":     "amazonecr",
	"amazonelasticcontainerregistry": "amazonecr",
	"amazonec2containerservice":      "amazonecs", // the old name of ECS
	"ecr":                            "amazonecr",

	// Short names, so a hand-written service map can say "lambda" or "s3".
	"lambda":        "awslambda",
	"ec2":           "amazonec2",
	"s3":            "amazons3",
	"rds":           "amazonrds",
	"aurora":        "amazonrds",
	"ecs":           "amazonecs",
	"eks":           "amazoneks",
	"dynamodb":      "amazondynamodb",
	"dynamo":        "amazondynamodb",
	"sqs":           "awsqueueservice",
	"sns":           "amazonsns",
	"cloudfront":    "amazoncloudfront",
	"elasticache":   "amazonelasticache",
	"kinesis":       "amazonkinesis",
	"opensearch":    "amazonopensearchservice",
	"elasticsearch": "amazonopensearchservice",
	"sagemaker":     "amazonsagemaker",
	"bedrock":       "amazonbedrock",
	"elb":           "amazonelasticloadbalancing",
	"alb":           "amazonelasticloadbalancing",
	"nlb":           "amazonelasticloadbalancing",
}

// CanonicalService returns the identity of a cloud service name: the same value
// for "AWS Lambda", "AWSLambda" and "lambda", and for "Amazon Simple Storage
// Service", "AmazonS3" and "s3". Unknown names reduce to a squashed form, so two
// spellings of a service this table does not know still agree when they differ
// only in case, spacing or punctuation.
func CanonicalService(name string) string {
	key := squash(name)
	if canonical, ok := serviceAliases[key]; ok {
		return canonical
	}
	return key
}

// squash lower-cases s and drops everything that is not a letter or digit.
func squash(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// minPartialLen is the shortest canonical name considered for a partial match,
// so a stray two-letter token does not "partially match" half of AWS.
const minPartialLen = 3

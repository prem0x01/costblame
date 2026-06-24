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

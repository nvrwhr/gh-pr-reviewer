package main

import "testing"

func TestParseStructuredReviewStripsMarkdownFence(t *testing.T) {
	out := "```json\n{\"body_markdown\":\"ok\",\"inline_comments\":[],\"recommendation\":\"comment\",\"confidence\":\"high\"}\n```"
	review, err := parseStructuredReview(out)
	if err != nil {
		t.Fatalf("parseStructuredReview error: %v", err)
	}
	if review.BodyMarkdown != "ok" {
		t.Fatalf("body = %q, want ok", review.BodyMarkdown)
	}
}

func TestNormalizeRecommendationLowConfidenceFailsClosed(t *testing.T) {
	if got := normalizeRecommendation("approve", "low"); got != "request_changes" {
		t.Fatalf("got %q, want request_changes", got)
	}
	if got := normalizeRecommendation("comment", "high"); got != "comment" {
		t.Fatalf("got %q, want comment", got)
	}
}

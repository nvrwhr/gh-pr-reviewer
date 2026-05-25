package main

import "testing"

func TestExtractRelatedIssueNumbers(t *testing.T) {
	text := `Fixes #12
Related to #9 and https://github.com/nvrwhr/gh-pr-reviewer/issues/42
This PR is #5 and should be ignored.`
	got := ExtractRelatedIssueNumbers(text, 5, 10)
	want := []int{9, 12, 42}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestExtractRelatedIssueNumbersCapsResults(t *testing.T) {
	got := ExtractRelatedIssueNumbers("refs #3 #1 #2", 0, 2)
	want := []int{1, 2}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v, want %v", got, want)
	}
}

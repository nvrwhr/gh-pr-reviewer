package main

import (
	"testing"

	"github.com/google/go-github/v55/github"
)

func TestBuildDiffContextMultiHunk(t *testing.T) {
	path := "main.go"
	patch := `@@ -1,3 +1,4 @@
 package main
+import "fmt"
 
 func main() {
@@ -10,3 +11,4 @@ func main() {
 	println("old")
+	fmt.Println("new")
 }`
	ctx := BuildDiffContext([]*github.CommitFile{{Filename: &path, Patch: &patch}})

	if !ctx.IsValidInlineTarget(path, 2) {
		t.Fatalf("line 2 should be a valid added line")
	}
	if !ctx.IsValidInlineTarget(path, 12) {
		t.Fatalf("line 12 should be a valid added line")
	}
	if ctx.IsValidInlineTarget(path, 1) {
		t.Fatalf("context line 1 must not be valid")
	}
	if ctx.IsValidInlineTarget(path, 11) {
		t.Fatalf("context line 11 must not be valid")
	}
}

func TestValidateReviewCommentsDropsInvalidAndDuplicates(t *testing.T) {
	path := "file.go"
	patch := `@@ -1,2 +1,3 @@
 package p
+func added() {}
 func old() {}`
	ctx := BuildDiffContext([]*github.CommitFile{{Filename: &path, Patch: &patch}})
	body := "comment"
	validLine := 2
	invalidLine := 1
	comments := []*github.DraftReviewComment{
		{Path: &path, Line: &validLine, Body: &body},
		{Path: &path, Line: &validLine, Body: &body},
		{Path: &path, Line: &invalidLine, Body: &body},
	}

	valid, dropped := ValidateReviewComments(comments, ctx)
	if len(valid) != 1 {
		t.Fatalf("got %d valid comments, want 1", len(valid))
	}
	if dropped != 2 {
		t.Fatalf("got %d dropped comments, want 2", dropped)
	}
}

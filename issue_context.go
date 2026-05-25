package main

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/google/go-github/v55/github"
)

type RelatedIssueContext struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"`
	URL    string `json:"url"`
	Body   string `json:"body"`
}

func FetchRelatedIssues(client *github.Client, ctx context.Context, owner, repo string, pr *github.PullRequest, max int) []RelatedIssueContext {
	if client == nil || pr == nil || max <= 0 {
		return nil
	}
	text := strings.Join([]string{pr.GetTitle(), pr.GetBody()}, "\n")
	numbers := ExtractRelatedIssueNumbers(text, pr.GetNumber(), max)
	if len(numbers) == 0 {
		log.Printf("Related issues: none referenced in PR title/body")
		return nil
	}

	log.Printf("Related issues: found referenced issue numbers %v", numbers)
	issues := make([]RelatedIssueContext, 0, len(numbers))
	for _, number := range numbers {
		issue, _, err := client.Issues.Get(ctx, owner, repo, number)
		if err != nil {
			log.Printf("Related issues: failed to fetch #%d (continuing): %v", number, err)
			continue
		}
		if issue.IsPullRequest() {
			log.Printf("Related issues: #%d is a PR, skipping as issue context", number)
			continue
		}
		issues = append(issues, RelatedIssueContext{
			Number: issue.GetNumber(),
			Title:  issue.GetTitle(),
			State:  issue.GetState(),
			URL:    issue.GetHTMLURL(),
			Body:   truncateString(issue.GetBody(), 8000),
		})
	}
	log.Printf("Related issues: fetched %d issue descriptions", len(issues))
	return issues
}

func ExtractRelatedIssueNumbers(text string, currentPRNumber int, max int) []int {
	if max <= 0 {
		return nil
	}
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(?i)(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?|refs?|references|related(?:\s+to)?|see)\s+(?:[\w.-]+/[\w.-]+)?#(\d+)`),
		regexp.MustCompile(`(?i)github\.com/[^\s/]+/[^\s/]+/issues/(\d+)`),
		regexp.MustCompile(`#(\d+)`),
	}
	seen := map[int]bool{}
	for _, pattern := range patterns {
		for _, match := range pattern.FindAllStringSubmatch(text, -1) {
			if len(match) < 2 {
				continue
			}
			n, err := strconv.Atoi(match[1])
			if err != nil || n <= 0 || n == currentPRNumber {
				continue
			}
			seen[n] = true
		}
	}
	numbers := make([]int, 0, len(seen))
	for n := range seen {
		numbers = append(numbers, n)
	}
	sort.Ints(numbers)
	if len(numbers) > max {
		numbers = numbers[:max]
	}
	return numbers
}

func RelatedIssuesMarkdown(issues []RelatedIssueContext) string {
	if len(issues) == 0 {
		return "No related issue descriptions found."
	}
	var b strings.Builder
	for _, issue := range issues {
		fmt.Fprintf(&b, "## Issue #%d: %s\nState: %s\nURL: %s\n\n%s\n\n", issue.Number, issue.Title, issue.State, issue.URL, issue.Body)
	}
	return b.String()
}

func truncateString(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("\n\n...[truncated %d bytes]", len(s)-max)
}

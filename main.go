package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-github/v55/github"
	"github.com/joho/godotenv"
	"github.com/sashabaranov/go-openai"
	"golang.org/x/oauth2"
)

func main() {
	// Load environment variables from .env file
	err := godotenv.Load()
	if err != nil {
		fmt.Println("Error loading .env file")
		os.Exit(1)
	}

	// Define command-line flags
	owner := flag.String("owner", "", "Repository owner (e.g., 'octocat')")
	repo := flag.String("repo", "", "Repository name (e.g., 'hello-world')")
	prNumber := flag.Int("pr", 0, "Pull Request number (e.g., 42)")
	dryRun := flag.Bool("dry", false, "Generate review without posting to GitHub")
	flag.Parse()

	// Check required arguments
	if *owner == "" || *repo == "" || *prNumber == 0 {
		fmt.Println("Usage: gh-pr-reviewer -owner=<owner> -repo=<repo> -pr=<pr-number> [--dry]")
		os.Exit(1)
	}

	// Initialize the GitHub client
	ctx := context.Background()
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: os.Getenv("GITHUB_TOKEN")},
	)
	tc := oauth2.NewClient(ctx, ts)
	client := github.NewClient(tc)

	// Fetch PR details
	pr, _, err := client.PullRequests.Get(ctx, *owner, *repo, *prNumber)
	if err != nil {
		fmt.Printf("Error fetching PR details: %v\n", err)
		os.Exit(1)
	}

	// Fetch PR checks (e.g., CI tests)
	checks, _, err := client.Checks.ListCheckRunsForRef(ctx, *owner, *repo, *pr.Head.SHA, &github.ListCheckRunsOptions{})
	if err != nil {
		fmt.Printf("Error fetching PR checks: %v\n", err)
		os.Exit(1)
	}

	// If any check has failed, do not allow approval
	checksPassed := true
	for _, check := range checks.CheckRuns {
		if check.GetConclusion() == "failure" {
			checksPassed = false
			break
		}
	}

	// Fetch PR files
	files, _, err := client.PullRequests.ListFiles(ctx, *owner, *repo, *prNumber, &github.ListOptions{})
	if err != nil {
		fmt.Printf("Error fetching PR files: %v\n", err)
		os.Exit(1)
	}

	// Generate the review and parse the assistant's recommendation
	review, reviewComments, action, err := generateReviewWithAssistant(pr, files)
	if err != nil {
		fmt.Printf("Error generating review: %v\n", err)
		os.Exit(1)
	}

	// Output the generated review
	fmt.Println("Generated Review:")
	fmt.Println(review)
	for _, comment := range reviewComments {
		fmt.Printf("File: %s, Line: %d\nComment: %s\n", *comment.Path, *comment.Position, *comment.Body)
	}

	if *dryRun {
		// If dry run, don't post the review, just output the results
		fmt.Println("Dry run: Review not posted to GitHub.")
		return
	}

	// Invalidate previous review by removing the stored review (optional)
	_ = invalidatePreviousReview(*prNumber)

	// Determine the action based on the assistant's recommendation and PR checks
	var state string
	if action == "approve" && checksPassed {
		state = "APPROVE"
	} else if action == "request_changes" || !checksPassed {
		state = "REQUEST_CHANGES"
	} else {
		fmt.Println("Assistant recommended approval, but tests are failing. Requesting changes instead.")
		state = "REQUEST_CHANGES"
	}

	// Post the review if not a dry run
	err = postReviewWithComments(client, ctx, *owner, *repo, *prNumber, review, reviewComments, state)
	if err != nil {
		fmt.Printf("Error posting review: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Review posted successfully!")
}

// generateReviewWithAssistant sends all file changes in a single prompt and generates a detailed review
func generateReviewWithAssistant(pr *github.PullRequest, files []*github.CommitFile) (string, []*github.DraftReviewComment, string, error) {
	if pr == nil {
		return "", nil, "", fmt.Errorf("No pull request to process")
	}

	client := openai.NewClient(os.Getenv("OPENAI_API_KEY"))

	body := ""
	title := ""
	author := ""

	if pr.Body != nil {
		body = *pr.Body
	}

	if pr.Title != nil {
		title = *pr.Title
	}

	if pr.User != nil && pr.User.Login != nil {
		author = *pr.User.Login
	}

	// Construct the full prompt with all file changes
	var fileChanges []string
	for _, file := range files {
		if file.Patch != nil {
			fileChanges = append(fileChanges, fmt.Sprintf("File: %s\nPatch:\n%s", *file.Filename, *file.Patch))
		}
	}

	combinedChanges := strings.Join(fileChanges, "\n\n")
	prompt := fmt.Sprintf(`
You are a code review assistant. A developer has submitted a pull request (PR) with the following details:

Title: %s
Author: %s
Description: %s

The following files were changed:
%s

Please provide a detailed code review that includes:
1. Summary of what the PR does.
2. Suggestions for improvements or refactoring.
3. Potential bugs or issues to look out for.
4. Comments on specific lines of code where necessary.

Finally, make a recommendation on whether this PR should be approved or if changes are required. Respond with "approve" or "request_changes" at the end of your review.
`, title, author, body, combinedChanges)

	resp, err := client.CreateChatCompletion(context.Background(), openai.ChatCompletionRequest{
		Model: openai.GPT4oMini,
		Messages: []openai.ChatCompletionMessage{
			{
				Role:    openai.ChatMessageRoleUser,
				Content: prompt,
			},
		},
		User: os.Getenv("ASSISTANT_ID"),
	})
	if err != nil {
		return "", nil, "", err
	}

	responseText := resp.Choices[0].Message.Content

	// Parse the response to determine the action (approve or request changes)
	var action string
	if strings.Contains(strings.ToLower(responseText), "approve") {
		action = "approve"
	} else if strings.Contains(strings.ToLower(responseText), "request_changes") {
		action = "request_changes"
	} else {
		action = "request_changes" // Default to requesting changes if unsure
	}

	// Example: Parse the response for line comments and overall feedback
	var reviewComments []*github.DraftReviewComment
	// Here you would parse `responseText` to extract specific line comments and general feedback
	// For simplicity, we'll just add a mock comment
	if len(responseText) > 0 {
		reviewComments = append(reviewComments, &github.DraftReviewComment{
			Path:     github.String("example.go"),
			Position: github.Int(12), // Example line number, this would be parsed from the response
			Body:     github.String("This line could benefit from better error handling."),
		})
	}

	return responseText, reviewComments, action, nil
}

func saveReviewToFile(prNumber int, review string) error {
	reviewDir := "reviews"
	if err := os.MkdirAll(reviewDir, os.ModePerm); err != nil {
		return err
	}

	reviewFile := filepath.Join(reviewDir, fmt.Sprintf("pr-%d.txt", prNumber))
	return os.WriteFile(reviewFile, []byte(review), 0644)
}

func loadReviewFromFile(prNumber int) (string, error) {
	reviewFile := filepath.Join("reviews", fmt.Sprintf("pr-%d.txt", prNumber))
	content, err := os.ReadFile(reviewFile)
	if err != nil {
		return "", err
	}
	return string(content), nil
}

func invalidatePreviousReview(prNumber int) error {
	reviewFile := filepath.Join("reviews", fmt.Sprintf("pr-%d.txt", prNumber))
	return os.Remove(reviewFile)
}

// postReviewWithComments posts a review on the PR with the determined action (approve or request changes), including line comments
func postReviewWithComments(client *github.Client, ctx context.Context, owner, repo string, prNumber int, review string, comments []*github.DraftReviewComment, state string) error {
	reviewEvent := &github.PullRequestReviewRequest{
		Body:     github.String(review),
		Event:    github.String(state),
		Comments: comments,
	}

	_, _, err := client.PullRequests.CreateReview(ctx, owner, repo, prNumber, reviewEvent)
	if err != nil {
		if ghErr, ok := err.(*github.ErrorResponse); ok && ghErr.Response.StatusCode == 422 {
			// Handle the "one pending review" scenario
			fmt.Println("A pending review already exists. Please submit or dismiss the existing review before posting a new one.")
			return nil
		}
		return err
	}
	return nil
}

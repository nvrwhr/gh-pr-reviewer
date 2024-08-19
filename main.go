package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
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

	// Fetch the current user (the reviewer)
	user, _, err := client.Users.Get(ctx, "")
	if err != nil {
		fmt.Printf("Error fetching user details: %v\n", err)
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

	// Check for pending reviews
	pendingReview, err := getPendingReview(client, ctx, *owner, *repo, *prNumber)
	if err != nil {
		fmt.Printf("Error checking for pending reviews: %v\n", err)
		os.Exit(1)
	}

	// Handle existing pending review
	if pendingReview != nil {
		fmt.Println("A pending review already exists.")
		if *dryRun {
			fmt.Println("Dry run: Review not posted to GitHub.")
			return
		}

		// Optionally, submit or dismiss the pending review here
		// For now, we'll dismiss it to proceed with the new review
		err = dismissPendingReview(client, ctx, *owner, *repo, *prNumber, pendingReview.GetID(), "Dismissing pending review to submit a new one.")
		if err != nil {
			fmt.Printf("Error dismissing pending review: %v\n", err)
			os.Exit(1)
		}
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

	// Check if the reviewer is the PR author
	isSelfReview := user.GetLogin() == pr.User.GetLogin()

	if isSelfReview {
		// Post the review as a comment instead
		commentBody := review
		if action == "approve" {
			commentBody += "\n\n**Note:** This is a self-approved PR."
		} else if action == "request_changes" {
			commentBody += "\n\n**Note:** This is a self-requested change."
		}

		comment := &github.IssueComment{
			Body: github.String(commentBody),
		}

		_, _, err := client.Issues.CreateComment(ctx, *owner, *repo, *prNumber, comment)
		if err != nil {
			fmt.Printf("Error posting comment: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Self-review posted as a comment.")
	} else {
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
}

// getPendingReview checks if there's a pending review for the PR
func getPendingReview(client *github.Client, ctx context.Context, owner, repo string, prNumber int) (*github.PullRequestReview, error) {
	reviews, _, err := client.PullRequests.ListReviews(ctx, owner, repo, prNumber, &github.ListOptions{})
	if err != nil {
		return nil, err
	}

	for _, review := range reviews {
		if review.GetState() == "PENDING" {
			return review, nil
		}
	}

	return nil, nil
}

// dismissPendingReview dismisses an existing pending review
func dismissPendingReview(client *github.Client, ctx context.Context, owner, repo string, prNumber int, reviewID int64, message string) error {
	_, _, err := client.PullRequests.DismissReview(ctx, owner, repo, prNumber, reviewID, &github.PullRequestReviewDismissalRequest{
		Message: github.String(message),
	})
	return err
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
	fileMap := make(map[string]*github.CommitFile)
	for _, file := range files {
		if file.Patch != nil {
			fileChanges = append(fileChanges, fmt.Sprintf("File: %s\nPatch:\n%s", *file.Filename, *file.Patch))
			fileMap[*file.Filename] = file
		}
	}

	combinedChanges := strings.Join(fileChanges, "\n\n")
	prompt := fmt.Sprintf(`
	PR %s by %s: %s
	
	The following files were changed:
	%s
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
	if strings.Contains(strings.ToLower(responseText), "__approve__") {
		action = "approve"
	} else if strings.Contains(strings.ToLower(responseText), "__request_changes__") {
		action = "request_changes"
	} else {
		action = "request_changes" // Default to requesting changes if unsure
	}

	// Example: Parse the response for line comments and overall feedback
	// var reviewComments []*github.DraftReviewComment

	// Mocked comment parsing logic; in reality, you would parse responseText
	// mockComment := "This line could benefit from better error handling."

	// for _, file := range files {
	// 	if file.Patch != nil {
	// 		// Ensure the file exists in the PR
	// 		if fileMap[*file.Filename] {
	// 			// Add a comment at a specific line, assuming line 12 for example
	// 			reviewComments = append(reviewComments, &github.DraftReviewComment{
	// 				Path:     file.Filename,
	// 				Position: github.Int(12), // Example line number, should be valid
	// 				Body:     github.String(mockComment),
	// 			})
	// 		}
	// 	}
	// }

	reviewComments, err := extractComments(responseText, fileMap)
	if err != nil {
		return "", nil, "", err
	}

	log.Println(`-------------------------------------Raw response`, responseText)
	log.Println(`-------------------------------------File comments`, len(reviewComments))

	return responseText, reviewComments, action, nil
}

func extractComments(responseText string, fileMap map[string]*github.CommitFile) ([]*github.DraftReviewComment, error) {
	var reviewComments []*github.DraftReviewComment

	// Example logic to parse responseText and map comments to files and lines
	// This logic should be adapted to match how the assistant's response is structured
	lines := strings.Split(responseText, "\n")
	for _, line := range lines {
		if strings.HasPrefix(strings.ToLower(line), "file:") {
			parts := strings.SplitN(line, ":", 3)
			if len(parts) == 3 {
				filePath := strings.TrimSpace(parts[1])
				if _, ok := fileMap[filePath]; ok {
					// Assuming the next line contains a comment with a line number
					// Example: "Line 12: This line could benefit from better error handling."
					if strings.Contains(lines[1], "Line") {
						commentParts := strings.SplitN(lines[1], ":", 2)
						if len(commentParts) == 2 {
							lineNumber, err := extractLineNumber(commentParts[0])
							if err == nil {
								reviewComments = append(reviewComments, &github.DraftReviewComment{
									Path:     github.String(filePath),
									Position: github.Int(lineNumber),
									Body:     github.String(strings.TrimSpace(commentParts[1])),
								})
							}
						}
					}
				}
			}
		}
	}

	return reviewComments, nil
}

// extractLineNumber extracts line number from a string like "Line 12"
func extractLineNumber(lineStr string) (int, error) {
	var lineNumber int
	_, err := fmt.Sscanf(lineStr, "Line %d", &lineNumber)
	return lineNumber, err
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
			fmt.Println("A pending review already exists. Please submit or dismiss the existing review before posting a new one: " + err.Error())
			return nil
		}
		fmt.Println("GH PR post Error: " + err.Error())
		return err
	}
	return nil
}

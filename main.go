package main

import (
	"context"
	"flag"
	"fmt"
	"os"

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
	approve := flag.Bool("approve", false, "Approve the PR")
	requestChanges := flag.Bool("request-changes", false, "Request changes to the PR")
	flag.Parse()

	// Check required arguments
	if *owner == "" || *repo == "" || *prNumber == 0 {
		fmt.Println("Usage: gh-pr-reviewer -owner=<owner> -repo=<repo> -pr=<pr-number> [--approve | --request-changes]")
		os.Exit(1)
	}

	// Initialize the GitHub client
	ctx := context.Background()
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: os.Getenv("GITHUB_TOKEN")},
	)
	tc := oauth2.NewClient(ctx, ts)
	client := github.NewClient(tc)

	// Get the Pull Request details
	pr, _, err := client.PullRequests.Get(ctx, *owner, *repo, *prNumber)
	if err != nil {
		fmt.Printf("Error fetching PR details: %v\n", err)
		os.Exit(1)
	}

	// Generate a review using the assistant with a specific ID
	review, err := generateReviewWithAssistant(pr)
	if err != nil {
		fmt.Printf("Error generating review: %v\n", err)
		os.Exit(1)
	}

	// Output the generated review
	fmt.Println("Generated Review:")
	fmt.Println(review)

	// Post the review if a flag is set
	if *approve || *requestChanges {
		err = postReview(client, ctx, *owner, *repo, *prNumber, review, *approve)
		if err != nil {
			fmt.Printf("Error posting review: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Review posted successfully!")
	}
}

// generateReviewWithAssistant sends PR details to the OpenAI assistant with a specific ID and gets a review summary
func generateReviewWithAssistant(pr *github.PullRequest) (string, error) {
	client := openai.NewClient(os.Getenv("OPENAI_API_KEY"))

	// Construct the prompt for the assistant
	prompt := fmt.Sprintf(`
You are a code review assistant. A developer has submitted a pull request (PR) with the following details:

Title: %s
Author: %s
Description: %s

Please provide a detailed code review that includes:
1. Summary of what the PR does.
2. Suggestions for improvements or refactoring.
3. Potential bugs or issues to look out for.
4. Any additional comments or concerns.

Respond as a detailed and professional code review.
`, *pr.Title, *pr.User.Login, *pr.Body)

	// Send the prompt to the assistant with the specific ID
	resp, err := client.CreateChatCompletion(context.Background(), openai.ChatCompletionRequest{
		// Model: os.Getenv("ASSISTANT_MODEL"),
		Messages: []openai.ChatCompletionMessage{
			{
				Role:    openai.ChatMessageRoleUser,
				Content: prompt,
			},
		},
		// Use the specific assistant ID
		User: os.Getenv("ASSISTANT_ID"),
	})
	if err != nil {
		return "", err
	}

	// Return the generated review
	return resp.Choices[0].Message.Content, nil
}

// postReview posts a review on the PR with either approval or request for changes
func postReview(client *github.Client, ctx context.Context, owner, repo string, prNumber int, review string, approve bool) error {
	var state string
	if approve {
		state = "APPROVE"
	} else {
		state = "REQUEST_CHANGES"
	}

	reviewEvent := &github.PullRequestReviewRequest{
		Body:  github.String(review),
		Event: github.String(state),
	}

	_, _, err := client.PullRequests.CreateReview(ctx, owner, repo, prNumber, reviewEvent)
	return err
}

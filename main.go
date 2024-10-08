package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/go-github/v55/github"
	"github.com/joho/godotenv"
	"github.com/sashabaranov/go-openai"
	"golang.org/x/oauth2"
)

type SavedReview struct {
	Review         string                       `json:"review"`
	ReviewComments []*github.DraftReviewComment `json:"review_comments"`
	Action         string                       `json:"action"`
}

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
	forcedry := flag.Bool("forcedry", false, "Force overwrite the last local dry run review")
	flag.Parse()

	// Check required arguments
	if *owner == "" || *repo == "" || *prNumber == 0 {
		fmt.Println("Usage: gh-pr-reviewer -owner=<owner> -repo=<repo> -pr=<pr-number> [--dry] [--forcedry]")
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

	// Construct the file path for the review
	reviewFilePath := fmt.Sprintf("reviews/%s-%s-review.json", *repo, *pr.Head.SHA)
	var savedReview *SavedReview

	// Check if a review file exists for the current head SHA
	if _, err := os.Stat(reviewFilePath); err == nil {
		// File exists, load the review from the file
		savedReview, err = loadReviewFromFile(reviewFilePath)
		if err == nil {
			log.Println("Using saved review from file.")
			logSavedReview(savedReview)

			if *dryRun {
				log.Println("Dry run: Review not posted to GitHub.")
				return
			}
		}
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

	var review string
	var reviewComments []*github.DraftReviewComment
	var action string

	// if there is no review, or we are forcing a new one
	if savedReview == nil || (forcedry != nil && *forcedry) {
		// ask LLM for review
		review, reviewComments, action, err = generateReviewWithAssistant(pr, files)
		if err != nil {
			fmt.Printf("Error generating review: %v\n", err)
			os.Exit(1)
		}

		// Output the generated review
		log.Println("------- Generated Review:")
		log.Println(review)
		log.Println("------- File comments:")
		for _, comment := range reviewComments {
			log.Printf("File: %s, Line: %d\nComment: %s\n", *comment.Path, *comment.Line, *comment.Body)
		}
		log.Println("-------")

	} else {
		review = savedReview.Review
		reviewComments = savedReview.ReviewComments
		action = savedReview.Action
	}

	if *dryRun || *forcedry {
		// Save the review to a file during dry run or after force
		err = saveReviewToFile(reviewFilePath, review, reviewComments, action)
		if err != nil {
			log.Printf("Error saving review to file: %v\n", err)
		}
		log.Println("Dry run: Review not posted to GitHub.")
		// either way the force or dry run END HERE <===================================
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

		// Use the PullRequests.CreateReview method to post review comments directly on lines
		reviewEvent := &github.PullRequestReviewRequest{
			Body:     github.String(commentBody),
			Event:    github.String("COMMENT"), // "COMMENT" will not change the state of the PR
			Comments: reviewComments,           // Use the existing review comments
		}

		_, _, err := client.PullRequests.CreateReview(ctx, *owner, *repo, *prNumber, reviewEvent)
		if err != nil {
			log.Fatalf("\n\n GH Review self-review comments: %v\n", err)
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
			log.Fatalf("Error posting review: %v\n", err)
		}
		fmt.Println("Review posted successfully!")
	}
}

func logSavedReview(savedReview *SavedReview) {
	log.Println("------- Loaded Review:")
	log.Println(savedReview.Review)
	log.Println("------- File comments:")
	for _, comment := range savedReview.ReviewComments {
		log.Printf("File: %s, Line: %d\nComment: %s\n", *comment.Path, *comment.Line, *comment.Body)
	}
	log.Println("-------")
}

func saveReviewToFile(reviewFilePath, review string, reviewComments []*github.DraftReviewComment, action string) error {
	// Save review content to .md file
	mdFilePath := strings.Replace(reviewFilePath, ".json", ".md", 1)
	err := os.WriteFile(mdFilePath, []byte(review), 0644)
	if err != nil {
		return fmt.Errorf("error saving review to .md file: %w", err)
	}

	// Save comments and action to .json file
	jsonFilePath := reviewFilePath
	savedReview := SavedReview{
		Review:         "", // Review content is stored in .md file
		ReviewComments: reviewComments,
		Action:         action,
	}
	data, err := json.MarshalIndent(savedReview, "", "  ")
	if err != nil {
		return fmt.Errorf("error marshaling review comments and action to JSON: %w", err)
	}
	err = os.WriteFile(jsonFilePath, data, 0644)
	if err != nil {
		return fmt.Errorf("error saving review comments and action to .json file: %w", err)
	}

	return nil
}

func loadReviewFromFile(reviewFilePath string) (*SavedReview, error) {
	// Load review content from .md file
	mdFilePath := strings.Replace(reviewFilePath, ".json", ".md", 1)
	reviewContent, err := os.ReadFile(mdFilePath)
	if err != nil {
		return nil, fmt.Errorf("error loading review content from .md file: %w", err)
	}

	// Load comments and action from .json file
	data, err := os.ReadFile(reviewFilePath)
	if err != nil {
		return nil, fmt.Errorf("error loading review comments and action from .json file: %w", err)
	}

	var savedReview SavedReview
	err = json.Unmarshal(data, &savedReview)
	if err != nil {
		return nil, fmt.Errorf("error unmarshaling review comments and action from JSON: %w", err)
	}

	// Replace the empty review content with the loaded content from the .md file
	savedReview.Review = string(reviewContent)

	return &savedReview, nil
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

func simplifyPatch(files []*github.CommitFile) string {
	var simplifiedChanges []string
	for _, file := range files {
		if file.Patch != nil {
			simplifiedChanges = append(simplifiedChanges, fmt.Sprintf("File: %s\nChanges:", *file.Filename))
			lines := strings.Split(*file.Patch, "\n")
			lineNumber := 0
			for _, line := range lines {
				if strings.HasPrefix(line, "@@") {
					// Extract line number from the diff header
					// For example, @@ -1,3 +1,3 @@ means we need to start at line 1
					parts := strings.Split(line, " ")
					if len(parts) >= 3 {
						newLineInfo := strings.Split(parts[2][1:], ",") // +1,3 becomes 1,3
						lineNumber, _ = strconv.Atoi(newLineInfo[0])
					}
				} else if strings.HasPrefix(line, "+") {
					simplifiedChanges = append(simplifiedChanges, fmt.Sprintf("+ Line %d: %s", lineNumber, strings.TrimPrefix(line, "+")))
					lineNumber++
				} else if strings.HasPrefix(line, "-") {
					simplifiedChanges = append(simplifiedChanges, fmt.Sprintf("- Line %d: %s", lineNumber, strings.TrimPrefix(line, "-")))
				} else {
					lineNumber++
				}
			}
		}
	}
	return strings.Join(simplifiedChanges, "\n")
}

// generateReviewWithAssistant sends all file changes in a single prompt and generates a detailed review
func generateReviewWithAssistant(pr *github.PullRequest, files []*github.CommitFile) (string, []*github.DraftReviewComment, string, error) {
	if pr == nil {
		return "", nil, "", fmt.Errorf("no pull request to process")
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
	simplifiedPatch := simplifyPatch(files)
	prompt := fmt.Sprintf(`
	PR %s by %s: %s
	
	The following files were changed:
	%s

	advanced diff:
	%s

	Summary of What the PR Does: (prettyfy this section)

Suggestions for Improvements or Refactoring: (prettyfy this section)

Potential Bugs or Issues to Look Out For: (prettyfy this section)

Specific Comments:

This section should contain specific comments on lines of code where you spot bugs, issues, or things that should be changed. Only include comments on problematic lines. Use the exact format provided below for each comment, and make sure to use double quotes around filenames and comments.

Format:
- File: "filename", Line line_number: "comment"

For multiple comments in the same file, use the format repeatedly for each line:

Example:
### Specific Comments:
- File: "fileA", Line 1: "comment a"
- File: "fileA", Line 2: "comment b"
- File: "fileB", Line 1: "comment c"

Ensure that:
The section header remains "### Specific Comments:".
The structure and formatting (e.g., double quotes around filenames and comments) are strictly followed.
Do not alter or omit the double quotes.
Each comment should start on a new line with the - symbol, followed by the word File, then the filename in double quotes, then the word Line, the line number, a colon, and finally the comment in double quotes.
Please adhere to the formatting rules strictly, as they are critical for automated processing.

Finally, make a recommendation on whether this PR should be approved or if changes are required. Respond with approve or request_changes at the end of your review.


	

Finally, make a recommendation on whether this PR should be approved or if changes are required. Respond with __approve__ or __request_changes__ at the end of your review.

	`, title, author, body, simplifiedPatch, combinedChanges)

	// fmt.Println(`----------------------------------------Combined changes`, simplifiedPatch, combinedChanges)

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

	reviewComments, err := extractComments(responseText, fileMap)
	if err != nil {
		return "", nil, "", err
	}
	log.Println(`------- Marked files for comments: `, len(reviewComments))
	responseText = removeSpecificCommentsSection(responseText)

	return responseText, reviewComments, action, nil
}

func removeSpecificCommentsSection(input string) string {
	// Define the regex pattern to match the section between:
	// 1. Headers with 1 to 4 `#` characters (e.g., `#### 4. Specific Comments`)
	// 2. Numbered titles with or without bold (e.g., `4. **Specific Comments:**` or `4. Specific Comments:`)
	// and the next `#{1,4} <some other section>` or `\d*\.?\s*\w+` (for numbered sections).
	pattern := `(?s)(?m)(#{1,4}\s+\d*\.?\s*\**Specific Comments\**:?.*?#{1,4}\s+\d*\.?\s*\w+|\d+\.\s*\*\*Specific Comments\*\*:?|\d+\.\s*Specific Comments:.*?#{1,4}\s+\d*\.?\s*\w+)`

	// Compile the regex pattern
	re := regexp.MustCompile(pattern)

	// Replace the matched section with the new section header, keeping the end section.
	cleaned := re.ReplaceAllStringFunc(input, func(m string) string {
		// Find the start of the next section to keep it intact
		nextSection := regexp.MustCompile(`#{1,4}\s+\d*\.?\s*\w+|\d+\.\s*\w+`).FindString(m)
		return nextSection
	})

	return cleaned
}

func extractComments(responseText string, fileMap map[string]*github.CommitFile) ([]*github.DraftReviewComment, error) {
	var reviewComments []*github.DraftReviewComment

	// Identify the start of the "Specific Comments" section
	specificCommentsIndex := strings.Index(responseText, "### Specific Comments:")
	if specificCommentsIndex == -1 {
		log.Println("No 'Specific Comments' section found")
		return reviewComments, nil
	}

	// Extract the "Specific Comments" section
	specificComments := responseText[specificCommentsIndex:]

	// Split the section into individual lines
	lines := strings.Split(specificComments, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)

		// Define regex to match the File, Line, and Comment format
		re := regexp.MustCompile(`- File: "([^"]+)", Line (\d+): "([^"]+)"`)

		if matches := re.FindStringSubmatch(line); matches != nil {
			filePart := matches[1]
			lineNumber, err := strconv.Atoi(matches[2])
			if err != nil {
				log.Printf("Invalid line number '%s' in line: %s", matches[2], line)
				continue
			}
			comment := matches[3]

			// Validate file part against the file map
			if _, exists := fileMap[filePart]; exists {
				reviewComments = append(reviewComments, &github.DraftReviewComment{
					Path: &filePart,
					Line: &lineNumber,
					Body: &comment,
				})
			} else {
				log.Printf("File %s not found in PR diff. Skipping comment.", filePart)
			}
		}
	}

	return reviewComments, nil
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

		log.Println("\n\n GH Review With Comments post Error: " + err.Error())
		return err
	}
	return nil
}

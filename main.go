package main

import (
	"context"
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
	log.Println("------- Generated Review:")
	log.Println(review)
	log.Println(`------- File comments:`)

	for _, comment := range reviewComments {
		log.Printf("File: %s, Line: %d\nComment: %s\n", *comment.Path, *comment.Position, *comment.Body)
	}

	log.Println(`-------`)

	if *dryRun {
		// If dry run, don't post the review, just output the results
		log.Println("Dry run: Review not posted to GitHub.")
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
			log.Fatal("Error posting review: %v\n", err)
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
	simplifiedPatch := simplifyPatch(files)
	prompt := fmt.Sprintf(`
	PR %s by %s: %s
	
	The following files were changed:
	%s

	advanced diff:
	%s

	Please provide a detailed code review that includes:
1. A summary of what the PR does (prettyfy this section).
2. Suggestions for improvements or refactoring (prettyfy this section).
3. Potential bugs or issues to look out for. In particular files (prettyfy this section).

4. Specific comments on lines of code where you spot bugs, issues, or things that should be changed. Include the file name and line number in your comments.

	Remember to provider specific comments on lines of code where you spot bugs, issues, or things that should be changed. Comment only of problematic lines. 
	Include the file name and line number in your comments, using the format 
	
	File: "<filename>", Line <line number>: "<comment>". 
	
	For mutliple lines please use just the first line number. If there are multiple comments in one file, please generate the comment multiple time as necessary, like that:

	### Specific Comments:
	- File: "fileA", Line 1: "comment a" 
	- File: "fileA", Line 2: "comment b"
	- File: "fileB", Line 1: "comment c" 
	...
	
	Do not change the header section name and structure. Addhere to file list structure, including the double quotes.
	So do not change the Specific Comment section name, do not change the comment quoting.


	

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
	specificCommentsIndex := strings.Index(responseText, "Specific Comments")
	if specificCommentsIndex == -1 {
		log.Println("------- no 'Specific Comments' section found")
		return reviewComments, nil
	}

	// Extract the "Specific Comments" section
	specificComments := responseText[specificCommentsIndex:]

	// Split the section into individual lines
	lines := strings.Split(specificComments, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		// Define regex to match different patterns
		re := regexp.MustCompile(`(?i)-?\s*\**File:\s*[` + "`" + `"']?([^,` + "`" + `"']+)['"]?,\s*Line\s*(\d+)\**:?\s*(.*)`)

		if matches := re.FindStringSubmatch(line); matches != nil {
			filePart := strings.TrimSpace(matches[1])
			lineNumberStr := strings.TrimSpace(matches[2])
			comment := strings.TrimSpace(matches[3])

			lineNumber, err := strconv.Atoi(lineNumberStr)
			if err != nil {
				log.Printf("Invalid line number '%s' in line: %s", lineNumberStr, line)
				continue
			}

			// Validate file and line number
			if _, exists := fileMap[filePart]; exists && lineNumber > 0 && comment != "" {
				reviewComments = append(reviewComments, &github.DraftReviewComment{
					Path:     &filePart,
					Position: &lineNumber,
					Body:     &comment,
				})
			} else {
				log.Printf("File %s not found in PR diff or invalid line/comment. Skipping comment.", filePart)
			}
		}
	}

	return reviewComments, nil
}

func parseLineComment(linePart string) (int, string) {
	// Extract the line number and comment from a line string
	lineNumberStr := ""
	comment := ""
	if strings.HasPrefix(linePart, "Line") {
		parts := strings.SplitN(linePart, ":", 2)
		if len(parts) == 2 {
			lineNumberStr = strings.TrimSpace(strings.TrimPrefix(parts[0], "Line "))
			comment = strings.TrimSpace(strings.TrimPrefix(strings.TrimSuffix(strings.TrimSpace(parts[1]), `"`), `"`))
		}
	}

	lineNumber, err := strconv.Atoi(lineNumberStr)
	if err != nil {
		return 0, ""
	}
	return lineNumber, comment
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

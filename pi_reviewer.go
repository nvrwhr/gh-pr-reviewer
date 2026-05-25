package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/go-github/v55/github"
)

type PiConfig struct {
	ContainerRepoPath string
	ContainerName     string
	Thinking          string
	Model             string
	UseCodeGraph      bool
}

type StructuredReview struct {
	BodyMarkdown   string `json:"body_markdown"`
	InlineComments []struct {
		Path     string `json:"path"`
		Line     int    `json:"line"`
		Body     string `json:"body"`
		Severity string `json:"severity"`
	} `json:"inline_comments"`
	Recommendation string `json:"recommendation"`
	Confidence     string `json:"confidence"`
}

func generateReviewWithPi(pr *github.PullRequest, files []*github.CommitFile, diffCtx *PullRequestDiffContext, relatedIssues []RelatedIssueContext, cfg PiConfig) (string, []*github.DraftReviewComment, string, error) {
	if pr == nil {
		return "", nil, "", fmt.Errorf("no pull request to process")
	}
	if cfg.ContainerRepoPath == "" {
		cfg.ContainerRepoPath = "/root/workspace"
	}
	if cfg.ContainerName == "" {
		cfg.ContainerName = "pi-sandbox"
	}
	if cfg.Thinking == "" {
		cfg.Thinking = "high"
	}

	log.Printf("Pi reviewer: using container=%q workdir=%q thinking=%q model=%q codegraph=%v", cfg.ContainerName, cfg.ContainerRepoPath, cfg.Thinking, cfg.Model, cfg.UseCodeGraph)
	log.Printf("Pi reviewer: PR title=%q files=%d related_issues=%d", pr.GetTitle(), len(files), len(relatedIssues))

	codeGraphContext := "CodeGraph disabled by reviewer. Do not run CodeGraph."
	if cfg.UseCodeGraph {
		codeGraphContext = "Use /skill:codegraph if available. CodeGraph is available inside the Pi container. Use it for affected-scope context only. Use the bash tool to run bounded CodeGraph commands only if useful, for example: codegraph status; codegraph sync .; codegraph affected <changed-file>; codegraph context \"review PR ...\" --format=markdown --max-nodes=20. If CodeGraph is not initialized or a command fails, mention that briefly and continue without it."
		log.Printf("Pi reviewer: CodeGraph prefetch disabled; Pi may invoke /skill:codegraph and CodeGraph via bash")
	} else {
		log.Printf("Pi reviewer: CodeGraph disabled")
	}

	prompt, err := buildPiReviewPrompt(pr, files, diffCtx, relatedIssues, codeGraphContext)
	if err != nil {
		return "", nil, "", err
	}

	promptFile, err := savePiPrompt(pr, prompt)
	if err != nil {
		return "", nil, "", err
	}
	log.Printf("Pi reviewer: prompt built (%d chars), saved to %s; invoking pi in container", len(prompt), promptFile)
	output, err := runPiPromptInContainer(cfg, promptFile)
	if err != nil {
		return "", nil, "", err
	}
	log.Printf("Pi reviewer: pi returned output (%d chars)", len(output))

	structured, err := parseStructuredReview(output)
	if err != nil {
		return "", nil, "", fmt.Errorf("pi returned invalid review JSON: %w\noutput:\n%s", err, output)
	}

	comments := make([]*github.DraftReviewComment, 0, len(structured.InlineComments))
	for _, c := range structured.InlineComments {
		path := c.Path
		line := c.Line
		body := strings.TrimSpace(c.Body)
		if c.Severity != "" {
			body = fmt.Sprintf("[%s] %s", c.Severity, body)
		}
		comments = append(comments, &github.DraftReviewComment{Path: &path, Line: &line, Body: &body})
	}
	comments, dropped := ValidateReviewComments(comments, diffCtx)
	log.Printf("Pi reviewer: parsed recommendation=%q confidence=%q inline_comments=%d dropped_invalid=%d", structured.Recommendation, structured.Confidence, len(comments), dropped)
	if dropped > 0 {
		structured.BodyMarkdown += fmt.Sprintf("\n\n_Note: dropped %d invalid or duplicate inline comment(s) that did not target valid changed PR lines._", dropped)
	}

	action := normalizeRecommendation(structured.Recommendation, structured.Confidence)
	return structured.BodyMarkdown, comments, action, nil
}

func buildPiReviewPrompt(pr *github.PullRequest, files []*github.CommitFile, diffCtx *PullRequestDiffContext, relatedIssues []RelatedIssueContext, codeGraphContext string) (string, error) {
	type promptFile struct {
		Path   string `json:"path"`
		Status string `json:"status"`
		Patch  string `json:"patch,omitempty"`
	}
	payload := struct {
		Title          string       `json:"title"`
		Body           string       `json:"body"`
		Author         string       `json:"author"`
		BaseSHA        string       `json:"base_sha"`
		HeadSHA        string       `json:"head_sha"`
		ValidLineTable string                `json:"valid_line_table"`
		RelatedIssues  []RelatedIssueContext `json:"related_issues"`
		Files          []promptFile          `json:"files"`
		CodeGraph      string                `json:"codegraph_affected_scope"`
	}{
		Title:          pr.GetTitle(),
		Body:           pr.GetBody(),
		Author:         pr.GetUser().GetLogin(),
		BaseSHA:        pr.GetBase().GetSHA(),
		HeadSHA:        pr.GetHead().GetSHA(),
		ValidLineTable: diffCtx.ValidLineSummary(),
		RelatedIssues:  relatedIssues,
		CodeGraph:      codeGraphContext,
	}
	for _, file := range files {
		if file == nil || file.Filename == nil {
			continue
		}
		payload.Files = append(payload.Files, promptFile{Path: file.GetFilename(), Status: file.GetStatus(), Patch: file.GetPatch()})
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", err
	}

	return `You are helping the PR author prepare a high-quality GitHub PR review/author note.
Return ONLY strict JSON with this schema:
{
  "body_markdown": "markdown summary with affected areas, risks, tests to run, and notes",
  "inline_comments": [{"path":"file", "line":123, "body":"comment", "severity":"bug|risk|nit|question"}],
  "recommendation": "approve|request_changes|comment",
  "confidence": "low|medium|high"
}

Rules:
- Inline comments may target ONLY the valid added lines listed in valid_line_table.
- Do not invent line numbers.
- Read the PR title/body and any related issue descriptions. Use them to explain intent, acceptance criteria, and user impact.
- Use CodeGraph affected scope to explain blast radius, affected tests, callers/callees, and risk when available.
- If instructed that CodeGraph is available, you may use bash to run at most 5 bounded codegraph commands. Do not spend excessive time on CodeGraph; continue if unavailable.
- Prefer recommendation "comment" for author/self-review style output unless there is a concrete blocker.
- If CI/test status is unknown, mention tests to run instead of claiming tests passed.

Input JSON:
` + string(data), nil
}

func savePiPrompt(pr *github.PullRequest, prompt string) (string, error) {
	if err := os.MkdirAll("reviews", 0755); err != nil {
		return "", fmt.Errorf("create reviews dir: %w", err)
	}
	repo := "repo"
	if pr.GetBase() != nil && pr.GetBase().GetRepo() != nil && pr.GetBase().GetRepo().GetName() != "" {
		repo = pr.GetBase().GetRepo().GetName()
	}
	sha := pr.GetHead().GetSHA()
	if sha == "" {
		sha = fmt.Sprintf("pr-%d", pr.GetNumber())
	}
	path := filepath.Join("reviews", fmt.Sprintf("%s-%s-prompt.md", sanitizePathPart(repo), sanitizePathPart(sha)))
	if err := os.WriteFile(path, []byte(prompt), 0644); err != nil {
		return "", fmt.Errorf("write pi prompt: %w", err)
	}
	return filepath.ToSlash(path), nil
}

func sanitizePathPart(s string) string {
	re := regexp.MustCompile(`[^A-Za-z0-9._-]+`)
	out := re.ReplaceAllString(s, "-")
	out = strings.Trim(out, "-.")
	if out == "" {
		return "unknown"
	}
	return out
}

func runPiPromptInContainer(cfg PiConfig, promptFile string) (string, error) {
	args := []string{
		"exec", "-i",
		"-w", cfg.ContainerRepoPath,
		"-e", "PI_PROJECT_DIR=" + cfg.ContainerRepoPath,
		cfg.ContainerName,
		"pi", "-p", "--no-session", "--tools", "read,grep,find,ls,bash", "--thinking", cfg.Thinking,
	}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	args = append(args, "@"+promptFile, "Return ONLY the strict JSON requested by the prompt file.")

	log.Printf("Pi reviewer: docker %s", shellQuoteArgs(args))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = append(os.Environ(), "MSYS_NO_PATHCONV=1", `MSYS2_ARG_CONV_EXCL=*`)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker exec pi failed against container %q: %w\nstderr:\n%s", cfg.ContainerName, err, stderr.String())
	}
	return stdout.String(), nil
}

func runToolInContainer(cfg PiConfig, tool string, args ...string) (string, error) {
	dockerArgs := []string{
		"exec", "-i",
		"-w", cfg.ContainerRepoPath,
		"-e", "PI_PROJECT_DIR=" + cfg.ContainerRepoPath,
		cfg.ContainerName,
		tool,
	}
	dockerArgs = append(dockerArgs, args...)
	log.Printf("CodeGraph: docker %s", shellQuoteArgs(dockerArgs))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", dockerArgs...)
	cmd.Env = append(os.Environ(), "MSYS_NO_PATHCONV=1", `MSYS2_ARG_CONV_EXCL=*`)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker exec %s failed against container %q: %w: %s", tool, cfg.ContainerName, err, stderr.String())
	}
	return stdout.String(), nil
}

func parseStructuredReview(output string) (*StructuredReview, error) {
	trimmed := strings.TrimSpace(output)
	if strings.HasPrefix(trimmed, "```") {
		trimmed = strings.TrimPrefix(trimmed, "```json")
		trimmed = strings.TrimPrefix(trimmed, "```")
		trimmed = strings.TrimSuffix(trimmed, "```")
		trimmed = strings.TrimSpace(trimmed)
	}
	start := strings.Index(trimmed, "{")
	end := strings.LastIndex(trimmed, "}")
	if start >= 0 && end > start {
		trimmed = trimmed[start : end+1]
	}
	var review StructuredReview
	if err := json.Unmarshal([]byte(trimmed), &review); err != nil {
		return nil, err
	}
	if strings.TrimSpace(review.BodyMarkdown) == "" {
		return nil, fmt.Errorf("body_markdown is empty")
	}
	return &review, nil
}

func shellQuoteArgs(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		if strings.ContainsAny(arg, " \t\n\"'") {
			quoted[i] = strconvQuote(arg)
		} else {
			quoted[i] = arg
		}
	}
	return strings.Join(quoted, " ")
}

func strconvQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func normalizeRecommendation(recommendation, confidence string) string {
	rec := strings.ToLower(strings.TrimSpace(recommendation))
	conf := strings.ToLower(strings.TrimSpace(confidence))
	if conf == "low" {
		return "request_changes"
	}
	switch rec {
	case "approve":
		return "approve"
	case "request_changes":
		return "request_changes"
	default:
		return "comment"
	}
}


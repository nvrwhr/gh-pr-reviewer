package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-github/v55/github"
)

type PiConfig struct {
	RepoPath          string
	ContainerRepoPath string
	Image             string
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

func generateReviewWithPi(pr *github.PullRequest, files []*github.CommitFile, diffCtx *PullRequestDiffContext, cfg PiConfig) (string, []*github.DraftReviewComment, string, error) {
	if pr == nil {
		return "", nil, "", fmt.Errorf("no pull request to process")
	}
	if cfg.RepoPath == "" {
		cfg.RepoPath = "."
	}
	if cfg.ContainerRepoPath == "" {
		cfg.ContainerRepoPath = "/root/workspace"
	}
	if cfg.Image == "" {
		cfg.Image = "pi-sandbox:latest"
	}
	if cfg.Thinking == "" {
		cfg.Thinking = "high"
	}

	absRepo, err := filepath.Abs(cfg.RepoPath)
	if err != nil {
		return "", nil, "", err
	}

	codeGraphContext := "CodeGraph disabled."
	if cfg.UseCodeGraph {
		codeGraphContext = buildCodeGraphContext(absRepo, cfg, pr, files)
	}

	prompt, err := buildPiReviewPrompt(pr, files, diffCtx, codeGraphContext)
	if err != nil {
		return "", nil, "", err
	}

	output, err := runPiPromptInDocker(absRepo, cfg, prompt)
	if err != nil {
		return "", nil, "", err
	}

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
	if dropped > 0 {
		structured.BodyMarkdown += fmt.Sprintf("\n\n_Note: dropped %d invalid or duplicate inline comment(s) that did not target valid changed PR lines._", dropped)
	}

	action := normalizeRecommendation(structured.Recommendation, structured.Confidence)
	return structured.BodyMarkdown, comments, action, nil
}

func buildPiReviewPrompt(pr *github.PullRequest, files []*github.CommitFile, diffCtx *PullRequestDiffContext, codeGraphContext string) (string, error) {
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
		ValidLineTable string       `json:"valid_line_table"`
		Files          []promptFile `json:"files"`
		CodeGraph      string       `json:"codegraph_affected_scope"`
	}{
		Title:          pr.GetTitle(),
		Body:           pr.GetBody(),
		Author:         pr.GetUser().GetLogin(),
		BaseSHA:        pr.GetBase().GetSHA(),
		HeadSHA:        pr.GetHead().GetSHA(),
		ValidLineTable: diffCtx.ValidLineSummary(),
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
- Use CodeGraph affected scope to explain blast radius, affected tests, callers/callees, and risk.
- Prefer recommendation "comment" for author/self-review style output unless there is a concrete blocker.
- If CI/test status is unknown, mention tests to run instead of claiming tests passed.

Input JSON:
` + string(data), nil
}

func runPiPromptInDocker(absRepo string, cfg PiConfig, prompt string) (string, error) {
	args := []string{
		"run", "--rm",
		"-v", fmt.Sprintf("%s:%s:rw", absRepo, cfg.ContainerRepoPath),
		"-e", "PI_PROJECT_DIR=" + cfg.ContainerRepoPath,
		"-w", cfg.ContainerRepoPath,
	}
	if envFileExists(filepath.Join(absRepo, ".env")) {
		args = append(args, "--env-file", filepath.Join(absRepo, ".env"))
	}
	args = append(args, cfg.Image, "pi", "-p", "--no-session", "--tools", "read,grep,find,ls", "--thinking", cfg.Thinking)
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	args = append(args, "Return the requested strict JSON for the PR review. Read the prompt from stdin.")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = append(os.Environ(), "MSYS_NO_PATHCONV=1", `MSYS2_ARG_CONV_EXCL=*`)
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("dockerized pi failed: %w\nstderr:\n%s", err, stderr.String())
	}
	return stdout.String(), nil
}

func buildCodeGraphContext(absRepo string, cfg PiConfig, pr *github.PullRequest, files []*github.CommitFile) string {
	var b strings.Builder
	status, err := runToolInDocker(absRepo, cfg, "codegraph", "status")
	if err != nil {
		return fmt.Sprintf("CodeGraph unavailable or uninitialized: %v", err)
	}
	fmt.Fprintf(&b, "## codegraph status\n%s\n", status)
	_, _ = runToolInDocker(absRepo, cfg, "codegraph", "sync", ".")

	changed := make([]string, 0, len(files))
	for _, file := range files {
		if file == nil || file.Filename == nil {
			continue
		}
		changed = append(changed, file.GetFilename())
		out, err := runToolInDocker(absRepo, cfg, "codegraph", "affected", file.GetFilename())
		if err == nil && strings.TrimSpace(out) != "" {
			fmt.Fprintf(&b, "\n## affected %s\n%s\n", file.GetFilename(), out)
		}
	}
	ctxQuery := fmt.Sprintf("review PR %s changed files %s", pr.GetTitle(), strings.Join(changed, ", "))
	out, err := runToolInDocker(absRepo, cfg, "codegraph", "context", ctxQuery, "--format=markdown", "--max-nodes=30")
	if err == nil && strings.TrimSpace(out) != "" {
		fmt.Fprintf(&b, "\n## task context\n%s\n", out)
	}
	return b.String()
}

func runToolInDocker(absRepo string, cfg PiConfig, tool string, args ...string) (string, error) {
	dockerArgs := []string{
		"run", "--rm",
		"-v", fmt.Sprintf("%s:%s:rw", absRepo, cfg.ContainerRepoPath),
		"-e", "PI_PROJECT_DIR=" + cfg.ContainerRepoPath,
		"-w", cfg.ContainerRepoPath,
		cfg.Image,
		tool,
	}
	dockerArgs = append(dockerArgs, args...)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", dockerArgs...)
	cmd.Env = append(os.Environ(), "MSYS_NO_PATHCONV=1", `MSYS2_ARG_CONV_EXCL=*`)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s failed: %w: %s", tool, err, stderr.String())
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

func envFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

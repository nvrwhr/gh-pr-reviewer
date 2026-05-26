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
	KeepPromptFile    bool
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

	if cfg.UseCodeGraph {
		log.Printf("Pi reviewer: CodeGraph enabled; Pi may invoke /skill:codegraph and bounded CodeGraph commands via bash")
	} else {
		log.Printf("Pi reviewer: CodeGraph disabled")
	}

	prompt, err := buildPiReviewPrompt(pr, files, diffCtx, relatedIssues, cfg.UseCodeGraph)
	if err != nil {
		return "", nil, "", err
	}

	promptFile, err := savePiPrompt(pr, prompt)
	if err != nil {
		return "", nil, "", err
	}
	if !cfg.KeepPromptFile {
		defer func() {
			if err := os.Remove(promptFile); err != nil && !os.IsNotExist(err) {
				log.Printf("Pi reviewer: failed to remove temp prompt file %s: %v", promptFile, err)
			}
		}()
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

func buildPiReviewPrompt(pr *github.PullRequest, files []*github.CommitFile, diffCtx *PullRequestDiffContext, relatedIssues []RelatedIssueContext, useCodeGraph bool) (string, error) {
	type promptFile struct {
		Path   string `json:"path"`
		Status string `json:"status"`
		Patch  string `json:"patch,omitempty"`
	}
	payload := struct {
		Title          string                `json:"title"`
		Body           string                `json:"body"`
		Author         string                `json:"author"`
		BaseSHA        string                `json:"base_sha"`
		HeadSHA        string                `json:"head_sha"`
		ValidLineTable string                `json:"valid_line_table"`
		RelatedIssues  []RelatedIssueContext `json:"related_issues"`
		Files          []promptFile          `json:"files"`
	}{
		Title:          pr.GetTitle(),
		Body:           pr.GetBody(),
		Author:         pr.GetUser().GetLogin(),
		BaseSHA:        pr.GetBase().GetSHA(),
		HeadSHA:        pr.GetHead().GetSHA(),
		ValidLineTable: diffCtx.ValidLineSummary(),
		RelatedIssues:  relatedIssues,
	}
	for _, file := range files {
		if file == nil || file.Filename == nil {
			continue
		}
		payload.Files = append(payload.Files, promptFile{Path: file.GetFilename(), Status: file.GetStatus(), Patch: file.GetPatch()})
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	return renderPiReviewPrompt(piReviewPromptTemplateData{
		UseCodeGraph: useCodeGraph,
		InputJSON:    string(data),
	})
}

func savePiPrompt(pr *github.PullRequest, prompt string) (string, error) {
	reviewsDir := projectPath(".reviews")
	if err := os.MkdirAll(reviewsDir, 0755); err != nil {
		return "", fmt.Errorf("create .reviews dir: %w", err)
	}

	repo := "repo"
	if pr.GetBase() != nil && pr.GetBase().GetRepo() != nil && pr.GetBase().GetRepo().GetName() != "" {
		repo = pr.GetBase().GetRepo().GetName()
	}
	sha := pr.GetHead().GetSHA()
	if sha == "" {
		sha = fmt.Sprintf("pr-%d", pr.GetNumber())
	}

	path := filepath.Join(reviewsDir, fmt.Sprintf("%s-%s-prompt.md", sanitizePathPart(repo), sanitizePathPart(sha)))
	if err := os.WriteFile(path, []byte(prompt), 0644); err != nil {
		return "", fmt.Errorf("write pi prompt: %w", err)
	}

	return filepath.ToSlash(filepath.Join(".reviews", filepath.Base(path))), nil
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

	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return nil, err
	}

	var review StructuredReview
	if v, ok := raw["body_markdown"]; ok {
		if err := json.Unmarshal(v, &review.BodyMarkdown); err != nil {
			return nil, fmt.Errorf("invalid body_markdown: %w", err)
		}
	} else {
		for _, alias := range []string{"body_markmarkdown", "bodyMarkdown", "body"} {
			if v, ok := raw[alias]; ok {
				if err := json.Unmarshal(v, &review.BodyMarkdown); err != nil {
					return nil, fmt.Errorf("invalid %s: %w", alias, err)
				}
				break
			}
		}
	}
	if v, ok := raw["inline_comments"]; ok {
		if err := json.Unmarshal(v, &review.InlineComments); err != nil {
			return nil, fmt.Errorf("invalid inline_comments: %w", err)
		}
	}
	if v, ok := raw["recommendation"]; ok {
		if err := json.Unmarshal(v, &review.Recommendation); err != nil {
			return nil, fmt.Errorf("invalid recommendation: %w", err)
		}
	}
	if v, ok := raw["confidence"]; ok {
		if err := json.Unmarshal(v, &review.Confidence); err != nil {
			return nil, fmt.Errorf("invalid confidence: %w", err)
		}
	}
	if strings.TrimSpace(review.BodyMarkdown) == "" {
		return nil, fmt.Errorf("body_markdown is empty or missing")
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


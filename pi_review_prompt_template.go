package main

import (
	"bytes"
	"fmt"
	"text/template"
)

type piReviewPromptTemplateData struct {
	UseCodeGraph bool
	InputJSON    string
}

const piReviewPromptTemplate = `Return ONLY strict JSON:
{"body_markdown":"...","inline_comments":[{"path":"file","line":123,"body":"comment","severity":"bug|risk|nit|question"}],"recommendation":"approve|request_changes|comment","confidence":"low|medium|high"}

Review process:
1. Start from the PR metadata and patches in Input.
2. Use git inside the container to understand the real code around the diff.
{{- if .UseCodeGraph }}
3. Use CodeGraph for affected symbols, callers/callees, related files, and likely affected tests.
{{- else }}
3. Do not use CodeGraph for this run.
{{- end }}

Rules:
- Use only lines from valid_line_table.
- Do not invent paths or lines.
- Prefer git for grounding: inspect changed files, surrounding context, recent history, and rename/move context with bounded read-only commands.
- You may run basic read-only git commands in the container (for example: git status, git diff, git show, git log). Do not push, pull, fetch, merge, rebase, commit, tag, or change remotes.
{{- if .UseCodeGraph }}
- You may run at most 5 bounded codegraph commands if available. Use it only for affected scope. If unavailable or uninitialized, continue without it.
{{- end }}
- Prefer recommendation "comment" unless there is a concrete blocker.
- If test status is unknown, say what to run.

Input:
{{.InputJSON}}
`

func renderPiReviewPrompt(data piReviewPromptTemplateData) (string, error) {
	tmpl, err := template.New("pi-review-prompt").Parse(piReviewPromptTemplate)
	if err != nil {
		return "", fmt.Errorf("parse pi review prompt template: %w", err)
	}

	var out bytes.Buffer
	if err := tmpl.Execute(&out, data); err != nil {
		return "", fmt.Errorf("render pi review prompt template: %w", err)
	}
	return out.String(), nil
}

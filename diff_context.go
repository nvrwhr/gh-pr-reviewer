package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/google/go-github/v55/github"
)

type FileDiffContext struct {
	Path          string         `json:"path"`
	Status        string         `json:"status"`
	Patch         string         `json:"patch,omitempty"`
	ChangedLines  []int          `json:"changed_lines"`
	LineToPosition map[int]int    `json:"line_to_position"`
	Hunks         []DiffHunkInfo `json:"hunks"`
}

type DiffHunkInfo struct {
	Header      string `json:"header"`
	OldStart    int    `json:"old_start"`
	OldCount    int    `json:"old_count"`
	NewStart    int    `json:"new_start"`
	NewCount    int    `json:"new_count"`
	StartPos    int    `json:"start_position"`
}

type PullRequestDiffContext struct {
	Files map[string]*FileDiffContext `json:"files"`
}

func BuildDiffContext(files []*github.CommitFile) *PullRequestDiffContext {
	ctx := &PullRequestDiffContext{Files: map[string]*FileDiffContext{}}
	for _, file := range files {
		if file == nil || file.Filename == nil {
			continue
		}
		path := file.GetFilename()
		fd := &FileDiffContext{
			Path:           path,
			Status:         file.GetStatus(),
			LineToPosition: map[int]int{},
		}
		if file.Patch != nil {
			fd.Patch = file.GetPatch()
			parseUnifiedPatchInto(fd)
		}
		ctx.Files[path] = fd
	}
	return ctx
}

func parseUnifiedPatchInto(fd *FileDiffContext) {
	lines := strings.Split(fd.Patch, "\n")
	newLine := 0
	position := 0
	seenChanged := map[int]bool{}

	for _, line := range lines {
		position++
		if strings.HasPrefix(line, "@@") {
			h := parseHunkHeader(line)
			h.StartPos = position
			fd.Hunks = append(fd.Hunks, h)
			newLine = h.NewStart
			continue
		}

		if strings.HasPrefix(line, `\ No newline at end of file`) {
			continue
		}

		switch {
		case strings.HasPrefix(line, "+"):
			if newLine > 0 {
				seenChanged[newLine] = true
				fd.LineToPosition[newLine] = position
			}
			newLine++
		case strings.HasPrefix(line, "-"):
			// Deleted lines do not advance the new-file line number.
		default:
			if newLine > 0 {
				newLine++
			}
		}
	}

	for line := range seenChanged {
		fd.ChangedLines = append(fd.ChangedLines, line)
	}
	sort.Ints(fd.ChangedLines)
}

func parseHunkHeader(header string) DiffHunkInfo {
	// Example: @@ -12,3 +14,8 @@ optional section
	h := DiffHunkInfo{Header: header}
	parts := strings.Split(header, " ")
	if len(parts) < 3 {
		return h
	}
	h.OldStart, h.OldCount = parseRange(strings.TrimPrefix(parts[1], "-"))
	h.NewStart, h.NewCount = parseRange(strings.TrimPrefix(parts[2], "+"))
	return h
}

func parseRange(s string) (start, count int) {
	pieces := strings.SplitN(s, ",", 2)
	start, _ = strconv.Atoi(pieces[0])
	count = 1
	if len(pieces) == 2 {
		count, _ = strconv.Atoi(pieces[1])
	}
	return start, count
}

func (ctx *PullRequestDiffContext) IsValidInlineTarget(path string, line int) bool {
	if ctx == nil || ctx.Files == nil {
		return false
	}
	fd := ctx.Files[path]
	if fd == nil {
		return false
	}
	_, ok := fd.LineToPosition[line]
	return ok
}

func (ctx *PullRequestDiffContext) ValidLineSummary() string {
	if ctx == nil || len(ctx.Files) == 0 {
		return "No changed-line context available."
	}
	paths := make([]string, 0, len(ctx.Files))
	for path := range ctx.Files {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	var b strings.Builder
	for _, path := range paths {
		fd := ctx.Files[path]
		if len(fd.ChangedLines) == 0 {
			fmt.Fprintf(&b, "- %s: no valid added-line inline comment targets\n", path)
			continue
		}
		fmt.Fprintf(&b, "- %s: valid added lines: %s\n", path, compactLineList(fd.ChangedLines, 80))
	}
	return b.String()
}

func compactLineList(lines []int, max int) string {
	if len(lines) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(lines))
	for i, line := range lines {
		if i >= max {
			parts = append(parts, fmt.Sprintf("... +%d more", len(lines)-max))
			break
		}
		parts = append(parts, strconv.Itoa(line))
	}
	return strings.Join(parts, ", ")
}

func ValidateReviewComments(comments []*github.DraftReviewComment, diffCtx *PullRequestDiffContext) ([]*github.DraftReviewComment, int) {
	valid := make([]*github.DraftReviewComment, 0, len(comments))
	seen := map[string]bool{}
	dropped := 0
	for _, comment := range comments {
		if comment == nil || comment.Path == nil || comment.Line == nil || comment.Body == nil || strings.TrimSpace(comment.GetBody()) == "" {
			dropped++
			continue
		}
		path := comment.GetPath()
		line := comment.GetLine()
		if !diffCtx.IsValidInlineTarget(path, line) {
			dropped++
			continue
		}
		key := fmt.Sprintf("%s:%d:%s", path, line, strings.TrimSpace(comment.GetBody()))
		if seen[key] {
			dropped++
			continue
		}
		seen[key] = true
		valid = append(valid, comment)
	}
	return valid, dropped
}

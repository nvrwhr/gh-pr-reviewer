# AGENTS.md

Project: `gh-pr-reviewer`

## Purpose

This tool generates GitHub PR review/author feedback from PR diffs. The upgrade direction is to produce better PR output by combining:

- GitHub PR metadata/checks/files.
- Correct unified-diff parsing and valid GitHub review line mapping.
- CodeGraph affected-scope context.
- Pi running inside the `pi-sandbox:latest` Docker container.

See `PLAN.md` for the active migration plan.

## Runtime Assumptions

Pi is not run directly on the host for this project. The Pi/CodeGraph container is started independently and the Go CLI connects to it with `docker exec`.

Start the container with a stable name:

```bash
export MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL="*"
ABS_PROJECT="$(pwd)"

docker run -it --rm \
  --name pi-sandbox \
  -v "${ABS_PROJECT}:/root/workspace:rw" \
  -e PI_PROJECT_DIR=/root/workspace \
  -w /root/workspace \
  pi-sandbox:latest
```

The reviewer invokes tools inside that running container, e.g.:

```bash
docker exec -i -w /root/workspace \
  -e PI_PROJECT_DIR=/root/workspace \
  pi-sandbox \
  pi -p --no-session --tools read,grep,find,ls,bash --thinking high "<prompt>"
```

Prefer baking required tools into `pi-sandbox:latest`:

- `pi`
- `codegraph`
- optional TypeScript/Node review runner
- any helper CLIs needed for review generation

Do not rely on host-installed Pi or CodeGraph for the final implementation. Do not start a new container per review; use the already-running container. Do not mount any extra directories from the reviewer process; assume the container was started on top of the project and `/root/workspace` is already the mounted project.

## High-Priority Implementation Rules

1. Fix diff grounding before improving LLM prompts.
   - The current simplified diff line logic can reference nonexistent or invalid lines.
   - Implement a real unified-diff parser.
   - Build an explicit valid inline-comment line map per file.
   - Validate every generated inline comment against this map before saving/posting.

2. CodeGraph is for affected scope and should be optional at runtime.
   - Use it to identify impacted symbols, callers/callees, dependent files, and affected tests.
   - Good commands:
     - `codegraph status`
     - `codegraph sync .`
     - `codegraph affected <file>`
     - `codegraph query <symbol> --json`
     - `codegraph impact <symbol>`
     - `codegraph callers <symbol>`
     - `codegraph callees <symbol>`
     - `codegraph context "review PR ..." --format=markdown --max-nodes=<n>`
   - If CodeGraph is missing/uninitialized, warn and continue without it.

3. Pi must receive bounded, structured context.
   - Include PR metadata, related issue descriptions when referenced, CI status, raw patch, parsed valid line map, and CodeGraph affected scope.
   - Tell Pi inline comments may target only provided valid lines.
   - Prefer strict JSON output over free-form markdown parsing.

4. Self-authored PRs must not submit state-changing reviews.
   - Use GitHub `COMMENT` for self-review mode.
   - Never self-approve/request-changes as an actual review state.

## Desired Review JSON Contract

Use strict JSON from Pi/model:

```json
{
  "body_markdown": "...",
  "inline_comments": [
    {
      "path": "main.go",
      "line": 123,
      "body": "...",
      "severity": "bug|risk|nit|question"
    }
  ],
  "recommendation": "approve|request_changes|comment",
  "confidence": "low|medium|high"
}
```

Validation requirements:

- `path` must be one of the PR files.
- `line` must be in the parsed valid changed-line map.
- `body` must be non-empty.
- duplicate comments should be removed.
- invalid comments should be dropped or moved into the body with a warning.

## Commands

Current dev commands:

```bash
go run . -owner=<owner> -repo=<repo> -pr=<number> -dry
go test ./...
```

If adding Node/Pi SDK runner code, keep commands documented in `README.md` and prefer running it inside `pi-sandbox:latest`.

## File Notes

- `main.go`: current single-file Go CLI.
- `.reviews/`: dry-run review cache keyed by repo/head SHA.
- `PLAN.md`: migration plan; update when architecture changes.
- `.env.example`: should document GitHub token and Dockerized Pi credential expectations.

## Coding Guidelines

- Keep GitHub API/posting logic separate from review generation.
- Add small interfaces/adapters:
  - GitHub adapter
  - diff parser/line mapper
  - CodeGraph adapter
  - Pi Docker adapter
  - review validator
- Prefer testable pure functions for diff parsing and response validation.
- Fail closed for posting: if unsure, use `COMMENT` or dry-run output, not invalid inline comments.
- Log context sources and counts: PR files, valid comment lines, CodeGraph affected files/tests/symbols, invalid generated comments.

# Plan: Upgrade `gh-pr-reviewer` for Pi + CodeGraph

## Goal

Turn the current PR-review generator into a PR-author assistant that creates higher-quality PR descriptions/reviews by combining:

- GitHub PR metadata, checks, changed files, and patch line mapping.
- CodeGraph semantic context: impacted symbols, callers/callees, affected tests, nearby architecture.
- Pi agent reasoning running inside the project Docker container via the Pi SDK or Pi RPC/print mode instead of a hard-coded OpenAI chat call.

The first target should preserve current behavior: dry-run files under `.reviews/`, GitHub posting, self-review fallback to `COMMENT`, and inline comments only on valid changed lines.

## Current State

- Single Go CLI in `main.go`.
- Fetches PR, checks, files, pending reviews via `go-github`.
- Builds one large prompt from simplified and raw diff.
- Current diff/line extraction is partially broken: simplified line numbers can drift and the model can reference lines that do not exist or are not valid GitHub review positions.
- Calls OpenAI directly through `github.com/sashabaranov/go-openai`.
- Parses free-form markdown for:
  - summary review body,
  - inline comments,
  - `approve` / `request_changes` action.
- Saves dry-run review as `.md` + `.json` keyed by PR head SHA.

## Proposed Architecture

```text
Go CLI
  ├─ GitHub adapter: PR metadata, checks, files, posting review
  ├─ Diff adapter: stable changed-line map + patches
  ├─ CodeGraph adapter: local semantic context from checked-out repo
  ├─ Pi adapter: sends structured review task to Pi
  └─ Review parser/validator: JSON schema, changed-line validation, save/post

Docker container (`pi-sandbox:latest`)
  └─ Pi agent running in `/root/workspace`
      ├─ project mounted read/write from host
      ├─ CodeGraph CLI baked into the image
      ├─ optional helper tools baked into the image
      ├─ `.codegraph/` index stored in the mounted project or container volume
      └─ review prompt specialized for PR-author output
```

Prefer a small adapter boundary so the Go CLI connects to the already-running `pi-sandbox` container used for real project work via `docker exec`. Bake Pi, CodeGraph, and review helper tools into that image, then run either `pi -p`, Pi RPC, or a small container-local runner inside `/root/workspace`.

## Phase 1 — Fix Diff and Review-Line Grounding

This must happen before Pi/CodeGraph improvements; otherwise better model context will still produce invalid GitHub comments.

1. Replace the current `simplifyPatch` line counter with a real unified-diff parser that emits:
   - file path,
   - hunks with old/new ranges,
   - added lines,
   - unchanged context lines,
   - valid GitHub review comment positions/lines.
2. Maintain two maps per file:
   - `changed_lines`: new-file line numbers where inline comments are allowed,
   - `review_positions`: GitHub diff positions when needed for API compatibility.
3. Never ask the model to invent line numbers from raw patches. Give it an explicit list/table of valid target lines per file.
4. Normalize edge cases:
   - renamed files,
   - deleted files (no inline comments unless GitHub supports the target),
   - binary files/no patch,
   - multi-hunk files,
   - no-newline markers,
   - comments on context lines vs added lines.
5. Validate generated comments against this map before saving/posting. Invalid comments become body notes or are dropped with a warning.

## Phase 2 — Stabilize Review Contract

1. Replace free-form parsing with a strict JSON response contract:

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

2. Keep markdown body separate from machine data.
3. Validate before saving/posting:
   - comment path is in PR files,
   - line is an added/changed line in the GitHub patch,
   - body is non-empty,
   - duplicate comments are removed.
4. Default to `comment` or `request_changes` when response is invalid or confidence is low.

## Phase 3 — Add CodeGraph Affected-Scope Context

1. Do not mount anything from the reviewer process. Assume the Pi container was already started on top of the project and the project is already mounted at `/root/workspace`.
2. Add a `-codegraph` flag, default enabled when `codegraph` exists on PATH.
3. Before review generation:
   - run `codegraph status`,
   - if not initialized, print setup guidance instead of silently failing,
   - run `codegraph sync .` when index exists.
4. Build affected scope per changed file. This is the main CodeGraph value:
   - `codegraph affected <file>` to identify affected tests and dependent files,
   - `codegraph impact <changed symbol>` to describe blast radius,
   - `codegraph callers <changed symbol>` and `codegraph callees <changed symbol>` for risky API/behavior changes,
   - `codegraph context "review PR <title> <changed files>" --format=markdown --max-nodes=<n>` for high-level task context.
5. Extract candidate changed symbols from the parsed diff and confirm them with `codegraph query` before running impact/caller/callee commands.
6. Add bounded context to the prompt:
   - changed files summary,
   - valid inline-comment line table from Phase 1,
   - affected scope: callers, callees, dependent files, affected tests,
   - relevant architectural context,
   - explicit note that inline comments must target only the provided valid PR diff lines.
7. Use CodeGraph scope to improve the PR body:
   - “Affected areas” section,
   - “Tests to run / affected tests” section,
   - “Risk and rollback notes” section.

## Phase 4 — Replace Direct OpenAI Call with Pi in Docker

Implement one of these adapters:

### Option A: Dockerized Pi CLI/RPC adapter from Go, fastest path

- Add `PiReviewer` that shells out to the already-running container, for example:

  ```bash
  docker exec -i \
    -w /root/workspace \
    -e PI_PROJECT_DIR=/root/workspace \
    pi-sandbox \
    pi -p --no-session --tools read,grep,find,ls,bash --thinking high "<review prompt>"
  ```

  or uses the same running container with `pi --mode rpc --no-session` for structured event handling.

- Match the existing manual launcher:

  ```bash
  docker run -it --rm \
    -v ${ABS_PROJECT}:/root/workspace:rw \
    -e PI_PROJECT_DIR=/root/workspace \
    -w /root/workspace \
    pi-sandbox:latest
  ```

- Required image contents/env:
  - Pi installed and authenticated/configured for the container runtime,
  - CodeGraph CLI baked into `pi-sandbox:latest`,
  - optional PR-review helper/runner baked into the image,
  - repo checkout already mounted at `/root/workspace` before the reviewer runs,
  - `.codegraph/` index available through repo mount or a named volume,
  - model provider credentials supplied through env or baked/mounted Pi auth.
- Benefits: minimal rewrite, keeps Go GitHub code, isolates Pi execution, matches actual runtime.
- Risks: subprocess/container lifecycle, stdout parsing, path translation between host and container, Docker availability in CI/local dev.

### Option B: TypeScript Pi SDK runner inside Docker, stronger integration

- Add `reviewer/` TypeScript package using `@mariozechner/pi-coding-agent`.
- Bake the Node runner, Pi package, and CodeGraph CLI into the Docker image.
- Go CLI gathers GitHub/diff/CodeGraph data and invokes the containerized Node runner with JSON stdin.
- Runner creates a Pi session with read-only tools and returns strict JSON.
- Benefits: direct SDK events, model selection, custom tools, cleaner long-term Pi integration, stable runtime dependencies.
- Risks: mixed Go/Node project, image build/publish flow, path translation, packaging complexity.

Recommended path: start with Option A using `docker exec` against the existing `pi-sandbox` container. Avoid depending on host-installed Pi/CodeGraph and avoid starting a fresh container per review. Design the adapter interface so Option B can replace it later.

## Phase 5 — Review Prompt Improvements

Create a dedicated prompt template that instructs Pi to act as the PR author, not an external reviewer:

- Explain what changed and why.
- Highlight risks, migration notes, and test evidence.
- Suggest follow-up improvements without blocking unnecessarily.
- Use inline comments only for concrete issues on changed lines.
- Do not approve if CI failed unless explicitly overridden.
- Prefer `comment` for self-review/author mode.

Inputs to include:

- PR title, body, author, branch, base/head SHA.
- Related issue descriptions when the PR body/title references issues (`Fixes #123`, `Refs #123`, issue URLs, etc.).
- CI/check summary.
- Raw patch + parsed valid-line map, not the old simplified line counter.
- CodeGraph affected scope: impacted symbols/files/tests plus callers/callees.
- Repository conventions from optional `AGENTS.md`/`CLAUDE.md` if present.

## Phase 6 — CLI and Config

Add flags/env:

- `-provider=pi|openai` temporarily, default `pi` after migration.
- `-pi-runtime=docker|local`, default `docker`.
- `-model=<pi model pattern>` passed to Pi when using CLI/RPC.
- `-thinking=off|minimal|low|medium|high|xhigh`.
- `-container-repo-path=/root/workspace` for the already-mounted project path inside the container.
- `-pi-container=pi-sandbox` for the already-running container name.
- `-pi-docker-tty=true|false`; false for automation, true for manual interactive debugging.
- `-pi-agent-dir=<path>` only if we decide not to keep Pi auth/config inside the sandbox image or mounted project.
- `-codegraph=true|false`.
- `-max-context-nodes=30`.
- `-author-mode=true|false`, default true when reviewer login equals PR author.

Update `.env.example`:

- keep `GITHUB_TOKEN`,
- remove or mark `OPENAI_API_KEY` as legacy,
- document that Pi auth/model config is managed by Pi inside the container (`/login`, env vars, or mounted `/root/.pi/agent/models.json`).

## Phase 7 — Tests

1. Unit tests:
   - unified diff parsing across multi-hunk/rename/delete/binary cases,
   - valid changed-line and GitHub review-position extraction,
   - response JSON parsing,
   - comment validation,
   - self-review state selection,
   - failed-check action downgrade.
2. Golden tests:
   - fixture PR patches + CodeGraph context → expected prompt shape.
3. Integration tests behind env flags:
   - dry run against a real PR,
   - CodeGraph disabled/enabled,
   - Docker unavailable gives clear error,
   - Pi container image missing gives clear build/run guidance,
   - Pi subprocess unavailable inside container gives clear error.

## Phase 8 — Rollout

1. Keep legacy OpenAI path behind `-provider=openai` until Pi path is stable.
2. Make `-dry` the recommended first run.
3. Log the exact context sources used:
   - PR files count,
   - CodeGraph status,
   - affected files/tests/symbols count,
   - invalid generated comment count,
   - Pi model/thinking level when available.
4. Save expanded prompt/context next to review files for debugging:
   - `.reviews/<repo>-<sha>-prompt.md`,
   - `.reviews/<repo>-<sha>-context.md`.

## Acceptance Criteria

- `go run . -owner=... -repo=... -pr=... -dry` generates a review through Pi running inside Docker.
- Diff parser produces a deterministic valid-line map for every patched file.
- If CodeGraph is initialized, review prompt includes affected scope: impacted files/tests/symbols and callers/callees where available.
- If CodeGraph is missing or uninitialized, the tool still works with a warning.
- Inline comments are never posted outside valid PR changed lines, even if Pi/model suggests invalid lines.
- Self-authored PRs are posted as `COMMENT`, never state-changing approval/request changes.
- Dry-run cache remains keyed by PR head SHA.
- README documents the existing `pi-sandbox:latest` launcher, baked-in Pi/CodeGraph tools, CodeGraph setup/index persistence, dry-run workflow, `/root/workspace` path mapping, credentials, and fallback behavior.

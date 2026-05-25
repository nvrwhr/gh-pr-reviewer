## Info

This tool generates GitHub Pull Request review/author feedback. It can create a general review body and inline comments on changed PR lines.

The default review provider is now Pi running inside `pi-sandbox:latest`. CodeGraph is used, when available in the container, to add affected-scope context such as dependent files/tests and relevant code context.

If you are the author of the PR, the tool only posts the review as a comment.

## Setup

Create a `.env` file based on `.env.example` with at least `GITHUB_TOKEN`.

Build the sandbox image if needed:

```bash
docker build -f Dockerfile.pi-sandbox -t pi-sandbox:latest .
```

Run/login to Pi inside the same sandbox shape used by the tool:

```bash
./scripts/run-pi-sandbox.sh .
```

Inside the container, authenticate/configure Pi as needed and initialize CodeGraph for the repo:

```bash
pi /login
codegraph init -i
```

## Example usage

```bash
go run . -owner=nvrwhr -repo=gh-pr-reviewer -pr=1 -dry
```

Legacy OpenAI path:

```bash
go run . -owner=nvrwhr -repo=gh-pr-reviewer -pr=1 -dry -provider=openai
```

## Arguments

```bash
gh-pr-reviewer -owner=<owner> -repo=<repo> -pr=<pr-number> [--dry] [--forcedry]
```

Useful Pi/Docker flags:

```bash
-provider=pi|openai              # default pi
-repo-path=.                     # host repo path to mount
-pi-image=pi-sandbox:latest      # image with pi + codegraph
-container-repo-path=/root/workspace
-model=<pi-model-pattern>
-thinking=off|minimal|low|medium|high|xhigh
-codegraph=true|false
```

## Dry/ForceDry Flags

If `-dry` is set, the tool creates review files based on the current head commit hash. Review them, then run again without `-dry` to post the saved review.

`-dry` reuses an existing review as long as the head commit does not change. Use `-forcedry` to generate a new review for the same head commit.

## Safety Notes

- Inline comments are validated against parsed valid changed lines before saving/posting.
- Invalid or duplicate model-generated comments are dropped.
- CodeGraph failures warn/degrade; review generation can continue without affected-scope context.
- Self-authored PRs are posted as `COMMENT`, never approval/request-changes state.

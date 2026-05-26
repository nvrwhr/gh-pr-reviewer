## Info

This tool generates GitHub Pull Request review/author feedback. It can create a general review body and inline comments on changed PR lines.

The default review provider is now Pi/CodeGraph inside an already-running `pi-sandbox` container. The Go CLI connects to that container with `docker exec`; it does not start a new container per review. The prompt includes the PR title/body and related GitHub issue descriptions when the PR references issues such as `Fixes #123`.

If you are the author of the PR, the tool only posts the review as a comment.

## Setup

Create a `.env` file based on `.env.example` with at least `GITHUB_TOKEN`.

Start the sandbox container independently and give it a stable name:

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

Inside that running container, authenticate/configure Pi as needed and initialize CodeGraph for the repo:

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
-pi-container=pi-sandbox         # running container name for docker exec
-container-repo-path=/root/workspace # mounted project path inside that container
-model=<pi-model-pattern>
-thinking=off|minimal|low|medium|high|xhigh
-codegraph=true|false            # default false; enable optional CodeGraph context
```

## Dry/ForceDry Flags

If `-dry` is set, the tool creates review files in `.reviews/` based on the current head commit hash. Review them, then run again without `-dry` to post the saved review.

`-dry` reuses an existing review as long as the head commit does not change. Use `-forcedry` to generate a new review for the same head commit.

## Safety Notes

- Inline comments are validated against parsed valid changed lines before saving/posting.
- Invalid or duplicate model-generated comments are dropped.
- CodeGraph failures warn/degrade; review generation can continue without affected-scope context.
- Self-authored PRs are posted as `COMMENT`, never approval/request-changes state.

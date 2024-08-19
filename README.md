## Info

This tool is a wrapper for the Agent to provide GitHub Pull Request code comments. It can generate a general review comment and create inline comments on specific lines of code if necessary.

If you are the author of the PR, the tool will only allow you to post the review as a comment.

To configure, create a .env file based on .env.example.

## Example usage

```
go run main.go -owner=nvrwhr  -repo=gh-pr-reviewer -pr=1 -dry
```

## Arguments

```
gh-pr-reviewer -owner=<owner> -repo=<repo> -pr=<pr-number> [--dry] [--forcedry]
```

## Dry/ForceDry Flags

If the `-dry` flag is set, the tool will create a review file based on the current head commit hash. You can review this file, and if you decide to apply the review, you can run the tool again without the `-dry` flag, and it will use the review from the file.

The `-dry` flag will prevent you from creating a new review as long as the head commit does not change. Use the `-forcedry` flag to trigger a new review even if the head commit hasn't changed.

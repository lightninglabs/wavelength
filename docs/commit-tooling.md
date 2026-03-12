# Commit Message Tooling

Use `client/scripts/commit_message.py` to lint, format, and safely reword
commit messages. The script enforces subject/body wrapping (`69`/`72`), keeps
real newlines, and preserves markdown-like body structure (lists, quotes, fenced
blocks, trailers).

## Common Workflows

```bash
# Lint the current commit message
python3 client/scripts/commit_message.py lint --commit HEAD

# Lint a commit range
python3 client/scripts/commit_message.py lint --range origin/master..HEAD

# Format a message file in place
python3 client/scripts/commit_message.py fmt --file /tmp/msg --in-place

# Reword a commit from formatted output
python3 client/scripts/commit_message.py reword --commit <sha>

# Preview a reword without rewriting history
python3 client/scripts/commit_message.py reword --commit <sha> --dry-run
```

## Literal Newlines

If a message was created with literal `\n` sequences, add
`--decode-escaped-newlines` to `fmt` or `reword` to convert them to real line
breaks.

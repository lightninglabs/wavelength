# Go Workspace Setup

This repository uses Go workspaces (`go.work`) to support multiple modules.

## Why

The `baselib/` directory is a separate Go module with its own `go.mod`. The
workspace enables:

1. **Targeted package testing**: `make unit pkg=baselib/actor`
2. **Cross-module imports**: Root can import baselib packages during development

## Files

- `go.work` - Committed. Links modules for development convenience.
- `go.work.sum` - Gitignored. Regenerated locally as needed.

## CI Behavior

CI sets `GOWORK=off` for static checks to test modules as external consumers
would see them (per Go team guidance). The `make unit` target tests all
submodules by cd'ing into each directory, which doesn't require the workspace.

## Setup

If the `go.work` file is accidentally deleted, recreate it:

```bash
go work init . ./baselib ./tools
```

## Usage

```bash
# Run all tests (root module + all submodules)
make unit

# Target a specific package (requires go.work for submodules)
make unit pkg=baselib/actor
make unit pkg=baselib/actor case=TestActorStartStop

# Target root module package
make unit pkg=db
```

## Adding New Submodules

If you add a new directory with its own `go.mod`:

```bash
go work use ./new-submodule
```

The `make unit` target auto-discovers submodules, so no Makefile changes needed.

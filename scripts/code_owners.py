#!/usr/bin/env python3
"""Generate the code-ownership map used to auto-assign incoming issues.

Ownership here means "who has been working on this lately", measured as
commits per author over a window. It is an input to triage, not a policy
file: nothing enforces it, and it does not gate review.

Each repository generates its OWN map, nightly, from its own history --
this same script lives in lumos, wavelength and swapdk-server. A repo is
the only place that can authoritatively answer who works on its code, and
having one repo compute the answer for another needed a cross-repo token
to do something that repo could do for itself.

Issues filed in lumos nevertheless track work in all three, so lumos's
nightly also folds in its siblings' published maps with --merge. That is
a fetch of one small file each, not a clone.

Both levels are emitted, per repository, and both are needed. Measured on
lumos over a twelve-month window, 174 of 617 tracked files -- 28% -- have
a different top author than the package containing them, so a
package-only map would misroute better than a quarter of the issues that
name a specific file. Package counts alone also flatten the margin: the
`indexer` package is 15-13 between two people, while `indexer/service.go`
inside it is 7-4, which is the difference between a coin-flip and a clear
answer.

Usage:
  python3 scripts/code_owners.py --check --repo wavelength=PATH \
                                         --repo swapdk-server=PATH
  python3 scripts/code_owners.py --write --repo wavelength=PATH \
                                         --repo swapdk-server=PATH
"""

import argparse
import collections
import json
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
DEFAULT_OUT = ROOT / ".github" / "code-owners.json"

# The ref each repository is measured on, by name. Named explicitly and
# never defaulting to HEAD: a checkout that gets reused sits on whatever
# branch ran last, and one wavelength clone seen in practice carried an
# `origin/master` holding lumos history -- so a bare `git log` there would
# silently have measured the wrong project.
REFS = {
    "lumos": "origin/master",
    "wavelength": "origin/main",
    "swapdk-server": "origin/master",
}

# git address -> GitHub handle. Keyed on address because names do not
# identify these people: the largest contributor's display name resembles
# their handle not at all, and two others each commit under more than one
# name. Keep in step with the same table in the issue-assign workflow.
AUTHOR_MAP = {
    "laolu32@gmail.com": "Roasbeef",
    "bhandras@gmail.com": "bhandras",
    "elle.mouton@gmail.com": "ellemouton",
    "kon@kon.ninja": "sputn1ck",
}

# How far back to look. Long enough to capture who built a subsystem,
# short enough that someone who moved off an area a year ago stops being
# its owner.
WINDOW = "12 months ago"

# Paths that tell you nothing about who should take an issue. Generated
# code and vendored trees have authors, but those authors are whoever ran
# the generator.
SKIP_PREFIXES = ("client/", "vendor/", "docs/")
SKIP_SUFFIXES = (".pb.go", ".pb.gw.go", "_generated.go", ".sql.go")

# Files below this many commits in the window are noise: a single drive-by
# edit would otherwise make someone the outright "owner" of a file. They
# still count toward their package, which is where thin evidence belongs.
MIN_FILE_COMMITS = 3


def origin_repo_name():
    """origin_repo_name derives the repo name from its origin remote.

    So the same script is byte-identical in all three repositories and
    needs no per-repo edit to say which one it is running in.
    """
    url = sh(["git", "remote", "get-url", "origin"], cwd=ROOT).strip()
    return url.rstrip("/").removesuffix(".git").split("/")[-1]


def sh(args, cwd):
    r = subprocess.run(args, cwd=cwd, capture_output=True, text=True)
    if r.returncode != 0:
        raise SystemExit(f"{' '.join(args[:6])}: {r.stderr.strip()[:200]}")
    return r.stdout


def tracked(path):
    """tracked reports whether a path should count toward ownership."""
    if not path.endswith(".go") or "/" not in path:
        return False
    if path.startswith(SKIP_PREFIXES) or path.endswith(SKIP_SUFFIXES):
        return False
    return True


def measure(repo_path, ref):
    """measure returns per-file and per-package author counts.

    One `git log` walk rather than one per path. The per-path form is
    O(paths) subprocesses and takes minutes on these repositories; this
    reads the same information in a single pass.

    --no-merges matters: a merge commit is attributed to whoever pressed
    the button, so counting them measures who merges, not who writes.
    """
    out = sh(["git", "log", ref, "--no-merges", f"--since={WINDOW}",
              "--format=%x00%ae", "--name-only"], cwd=repo_path)

    per_file = collections.defaultdict(collections.Counter)
    per_pkg = collections.defaultdict(collections.Counter)

    handle, pkgs_this_commit = None, set()

    def close_commit():
        # A package is credited ONCE per commit, however many of its files
        # that commit touched. Summing file touches instead would weigh a
        # single sweeping rename above months of focused work, and would
        # contradict the rule this map feeds -- commits, not lines.
        for pkg in pkgs_this_commit:
            per_pkg[pkg][handle] += 1

    for line in out.splitlines():
        if line.startswith("\x00"):
            if handle:
                close_commit()
            handle, pkgs_this_commit = AUTHOR_MAP.get(line[1:].strip()), set()
            continue
        if handle and line and tracked(line):
            per_file[line][handle] += 1
            pkgs_this_commit.add(line.split("/")[0])
    if handle:
        close_commit()

    return per_file, per_pkg


def shape(counts):
    """shape orders a counter most-active-first for a stable diff."""
    return dict(sorted(counts.items(), key=lambda kv: (-kv[1], kv[0])))


def merge(doc, paths):
    """merge folds sibling repositories' published maps into doc.

    Each entry keeps the `head` and `generated` of the repo that produced
    it, so a stale sibling is visible as an old commit rather than
    silently presented as current.
    """
    for path in paths:
        p = Path(path)
        if not p.exists():
            # A sibling that has not adopted this yet is expected, not an
            # error -- but it must be visible, because the consequence is
            # that issues about that repo have no ownership data at all.
            print(f"note: {path} not found; its repo will be absent "
                  f"from the map", file=sys.stderr)
            continue
        other = json.loads(p.read_text())
        for name, entry in other.get("repos", {}).items():
            if name in doc["repos"]:
                raise SystemExit(f"{path}: refusing to overwrite the "
                                 f"locally-measured {name!r}")
            doc["repos"][name] = entry
    return doc


def build(repos):
    doc = {
        "_comment": "Generated by scripts/code_owners.py -- do not edit. "
                    "Commits per author over the window; a tie-break "
                    "input for issue triage, not a policy or review gate.",
        "window": WINDOW,
        "repos": {},
    }
    for name, cfg in repos.items():
        per_file, per_pkg = measure(cfg["path"], cfg["ref"])
        doc["repos"][name] = {
            "ref": cfg["ref"],
            "head": sh(["git", "rev-parse", cfg["ref"]],
                       cwd=cfg["path"]).strip(),
            "packages": {p: shape(c) for p, c in sorted(per_pkg.items())},
            "files": {
                f: shape(c) for f, c in sorted(per_file.items())
                if sum(c.values()) >= MIN_FILE_COMMITS
            },
        }
    return doc


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--write", action="store_true")
    ap.add_argument("--check", action="store_true")
    ap.add_argument("--out", default=str(DEFAULT_OUT))
    ap.add_argument("--repo", default=None, metavar="NAME",
                    help="name of the repo being measured; defaults to the "
                         f"checkout's own. Known: {', '.join(sorted(REFS))}")
    ap.add_argument("--merge", action="append", default=[], metavar="FILE",
                    help="a sibling repo's published code-owners.json to "
                         "fold in. Repeatable. Used by lumos, whose issues "
                         "track work in all three repos.")
    args = ap.parse_args()
    if not (args.write or args.check):
        print(__doc__)
        return 1

    name = args.repo or origin_repo_name()
    if name not in REFS:
        raise SystemExit(f"unknown repo {name!r}; expected one of "
                         f"{sorted(REFS)}. Pass --repo to override.")

    doc = merge(build({name: {"ref": REFS[name], "path": str(ROOT)}}),
                args.merge)
    text = json.dumps(doc, indent=1, sort_keys=False) + "\n"

    for name, r in doc["repos"].items():
        print(f"{name} @ {r['head'][:8]}: {len(r['packages'])} packages, "
              f"{len(r['files'])} files")

    if args.check:
        cur = Path(args.out)
        if cur.exists() and cur.read_text() == text:
            print("map is current")
            return 0
        print("map is STALE" if cur.exists() else "map does not exist")
        return 1

    Path(args.out).write_text(text)
    print(f"wrote {args.out} ({len(text) // 1024} KiB)")
    return 0


if __name__ == "__main__":
    sys.exit(main())

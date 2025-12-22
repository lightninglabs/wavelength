#!/usr/bin/env python3

"""
comment_ratio computes a comment-density metric for Go source files.

Features:
- Count comment-only lines vs non-empty code lines for files or paths.
- Limit scope to files changed since a git ref.
- Print top files by comment ratio.
- Print a recursive package tree with per-package ratios.
- Include tests or restrict counts to *_test.go files.

Why this matters:
- Highlights under-documented areas quickly without full parsing overhead.
- Makes it easy to compare coverage across packages and test code paths.

Notes:
- Lines with code + trailing comment count as code.
- This is a lightweight lexer and does not fully parse Go string literals.
"""

from __future__ import annotations

import argparse
import os
import re
import subprocess
from typing import Generator
from pathlib import Path


WHITESPACE_RE = re.compile(r"\s+")


def sh(cmd: list[str], cwd: Path | str | None = None) -> str:
    return subprocess.check_output(
        cmd,
        cwd=cwd,
        text=True,
        stderr=subprocess.STDOUT,
    ).strip()


def is_git_repo(cwd: str) -> bool:
    try:
        sh(["git", "rev-parse", "--is-inside-work-tree"], cwd=cwd)
        return True
    except subprocess.CalledProcessError:
        return False


def include_go_file(
    path: Path,
    include_tests: bool,
    tests_only: bool,
) -> bool:
    if path.suffix != ".go":
        return False

    is_test = path.name.endswith("_test.go")
    if tests_only:
        return is_test
    if include_tests:
        return True
    return not is_test


def _walk_go_files(
    paths: list[str],
    include_tests: bool,
    tests_only: bool,
) -> Generator[Path, None, None]:
    for p in paths:
        path = Path(p).resolve()

        if path.is_dir():
            for go_file in path.rglob("*.go"):
                if include_go_file(
                    go_file,
                    include_tests=include_tests,
                    tests_only=tests_only,
                ):
                    yield go_file
            continue

        elif path.is_file() and include_go_file(
            path,
            include_tests=include_tests,
            tests_only=tests_only,
        ):
            yield path


def go_files_under(
    paths: list[str],
    include_tests: bool,
    tests_only: bool,
) -> list[Path]:
    files = {
        go_file
        for go_file in _walk_go_files(
            paths,
            include_tests=include_tests,
            tests_only=tests_only,
        )
    }

    return sorted(files, key=lambda p: str(p))


def _filter_git_output(
    output: str,
    repo_cwd: Path,
    include_tests: bool,
    tests_only: bool,
) -> list[Path]:
    files: set[Path] = set()

    for line in output.splitlines():
        rel = line.strip()
        if not rel:
            continue

        path = repo_cwd / rel
        if not path.exists():
            continue

        if include_go_file(
            path,
            include_tests=include_tests,
            tests_only=tests_only,
        ):
            files.add(path)

    return sorted(files, key=lambda p: str(p))


def go_files_changed(
    repo_cwd: Path,
    base_ref: str,
    include_tests: bool,
    tests_only: bool,
) -> tuple[str, list[Path]]:
    base = sh(
        ["git", "merge-base", base_ref, "HEAD"],
        cwd=repo_cwd,
    )
    out = sh(
        ["git", "diff", "--name-only", f"{base}..HEAD", "--", "*.go"],
        cwd=repo_cwd,
    )
    files = _filter_git_output(
        out,
        repo_cwd,
        include_tests=include_tests,
        tests_only=tests_only,
    )

    return base, files


def go_files_diff(
    repo_cwd: Path,
    left_ref: str,
    right_ref: str,
    include_tests: bool,
    tests_only: bool,
) -> tuple[str, list[Path]]:
    out = sh(
        [
            "git",
            "diff",
            "--name-only",
            f"{left_ref}..{right_ref}",
            "--",
            "*.go",
        ],
        cwd=repo_cwd,
    )
    files = _filter_git_output(
        out,
        repo_cwd,
        include_tests=include_tests,
        tests_only=tests_only,
    )

    return f"{left_ref}..{right_ref}", files


def go_files_staged(
    repo_cwd: Path,
    include_tests: bool,
    tests_only: bool,
) -> list[Path]:
    out = sh(
        ["git", "diff", "--name-only", "--cached", "--", "*.go"],
        cwd=repo_cwd,
    )
    return _filter_git_output(
        out,
        repo_cwd,
        include_tests=include_tests,
        tests_only=tests_only,
    )


def go_files_in_commit(
    repo_cwd: Path,
    commit_ref: str,
    include_tests: bool,
    tests_only: bool,
) -> list[Path]:
    out = sh(
        ["git", "show", "--name-only", "--pretty=", commit_ref, "--", "*.go"],
        cwd=repo_cwd,
    )
    return _filter_git_output(
        out,
        repo_cwd,
        include_tests=include_tests,
        tests_only=tests_only,
    )


def upstream_ref(repo_cwd: Path) -> str:
    return sh(
        [
            "git",
            "rev-parse",
            "--abbrev-ref",
            "--symbolic-full-name",
            "@{upstream}",
        ],
        cwd=repo_cwd,
    )

def _format_git_error(exc: subprocess.CalledProcessError) -> str:
    cmd = exc.cmd
    if isinstance(cmd, list):
        cmd_str = " ".join(cmd)
    else:
        cmd_str = str(cmd)

    output = exc.output.strip() if exc.output else ""
    if output:
        return f"Git command failed: {cmd_str}\n{output}"
    return f"Git command failed: {cmd_str}"


def _find_raw_string_end(s: str, start_index: int) -> int:
    end = s.find("`", start_index)
    return end if end != -1 else -1


def _find_string_end(s: str, start_index: int) -> int:
    end = start_index
    while True:
        next_quote = s.find("\"", end)
        if next_quote == -1:
            return -1

        backslashes = 0
        k = next_quote - 1
        while k >= 0 and s[k] == "\\":
            backslashes += 1
            k -= 1

        if backslashes % 2 == 0:
            return next_quote

        end = next_quote + 1


def count_go_file(path: Path) -> tuple[int, int]:
    """
    count_go_file returns (comment_only_lines, code_nonempty_lines).

    Comment-only:
    - A line that contains only comment text (line comment or a block comment
      line) plus optional whitespace.

    Code non-empty:
    - A line that contains any non-whitespace code outside comments.
    """

    comment_only = 0
    code_nonempty = 0
    in_block = False
    in_raw_string = False
    in_string = False

    with open(path, "r", encoding="utf-8", errors="replace") as f:
        for raw in f:
            s = raw.rstrip("\n")
            i = 0
            had_code = False
            had_comment = False

            while i < len(s):
                if in_block:
                    end = s.find("*/", i)
                    if end == -1:
                        had_comment = True
                        i = len(s)
                        break

                    had_comment = True
                    i = end + 2
                    in_block = False
                    continue

                if in_raw_string:
                    end = _find_raw_string_end(s, i)
                    if end < 0:
                        had_code = True
                        i = len(s)
                        break

                    had_code = True
                    i = end + 1
                    in_raw_string = False
                    continue

                if in_string:
                    end = _find_string_end(s, i)
                    if end < 0:
                        had_code = True
                        i = len(s)
                        break

                    had_code = True
                    i = end + 1
                    in_string = False
                    continue

                whitespace = WHITESPACE_RE.match(s, i)
                if whitespace:
                    i = whitespace.end()
                    continue

                if s.startswith("//", i):
                    had_comment = True
                    i = len(s)
                    break

                if s.startswith("/*", i):
                    had_comment = True
                    in_block = True
                    i += 2
                    continue

                if s.startswith("`", i):
                    had_code = True
                    in_raw_string = True
                    i += 1
                    continue

                if s.startswith("\"", i):
                    had_code = True
                    in_string = True
                    i += 1
                    continue

                had_code = True
                i += 1

            if had_code:
                code_nonempty += 1
            elif had_comment:
                comment_only += 1

    return comment_only, code_nonempty


def summarize(files: list[Path], repo_root: Path | None, top: int) -> None:
    total_comment = 0
    total_code = 0
    per_file: list[tuple[float, str, int, int]] = []

    for f in files:
        comment_lines, code_lines = count_go_file(f)
        total_comment += comment_lines
        total_code += code_lines

        rel = (
            os.path.relpath(str(f), str(repo_root))
            if repo_root
            else str(f)
        )
        ratio = float("inf") if code_lines == 0 else comment_lines / code_lines
        per_file.append((ratio, rel, comment_lines, code_lines))

    per_file.sort(reverse=True)
    ratio_total = 0.0 if total_code == 0 else total_comment / total_code

    print(f"Files: {len(files)}")
    print(f"Comment-only lines: {total_comment}")
    print(f"Non-comment non-empty lines: {total_code}")
    print(
        "Ratio (comment-only / code): "
        f"{ratio_total:.4f} ({ratio_total * 100:.1f}%)"
    )
    print()

    print(f"Top {min(top, len(per_file))} files by ratio:")
    for _, rel, comment_lines, code_lines in per_file[:top]:
        if code_lines == 0:
            ratio = "inf"
        else:
            ratio = f"{comment_lines / code_lines:.3f}"

        print(
            f"- {rel}: {comment_lines} comment-only, {code_lines} code "
            f"(ratio: {ratio})"
        )


def package_stats_tree(
    paths: list[str],
    repo_root: Path | None,
    include_tests: bool,
    tests_only: bool,
) -> tuple[dict[Path, tuple[int, int]], int, int]:
    stats: dict[Path, tuple[int, int]] = {}
    total_comment = 0
    total_code = 0

    grouped: dict[Path, list[Path]] = {}
    for go_file in _walk_go_files(
        paths,
        include_tests=include_tests,
        tests_only=tests_only,
    ):
        grouped.setdefault(go_file.parent, []).append(go_file)

    for package_dir, files in grouped.items():
        comment_lines = 0
        code_lines = 0
        for go_file in files:
            file_comment, file_code = count_go_file(go_file)
            comment_lines += file_comment
            code_lines += file_code

        total_comment += comment_lines
        total_code += code_lines

        rel = (
            package_dir.relative_to(repo_root)
            if repo_root
            else package_dir
        )
        stats[rel] = (comment_lines, code_lines)

    return stats, total_comment, total_code


def print_tree(
    stats: dict[Path, tuple[int, int]],
) -> None:
    tree: dict[str, dict] = {"children": {}, "stats": None}

    for rel_path, pkg_stats in stats.items():
        parts = [p for p in rel_path.parts if p and p != "."]
        if not parts:
            parts = ["."]
        node = tree
        for part in parts:
            node = node["children"].setdefault(
                part,
                {"children": {}, "stats": None},
            )
        node["stats"] = pkg_stats

    def walk(node: dict, prefix: str, is_last: bool) -> None:
        names = sorted(node["children"])
        for i, name in enumerate(names):
            child = node["children"][name]
            last = i == len(names) - 1
            branch = "`-- " if last else "|-- "
            line = f"{prefix}{branch}{name}"
            if child["stats"] is None:
                print(line)
            else:
                comment_lines, code_lines = child["stats"]
                if code_lines == 0:
                    ratio = "inf"
                else:
                    ratio = f"{comment_lines / code_lines:.3f}"
                print(
                    f"{line}: {comment_lines} comment-only, "
                    f"{code_lines} code (ratio: {ratio})"
                )
            child_prefix = f"{prefix}{'    ' if last else '|   '}"
            walk(child, child_prefix, last)

    walk(tree, "", True)


def main() -> None:
    parser = argparse.ArgumentParser(
        description="Compute Go comment-only vs code non-empty line ratio."
    )
    parser.add_argument(
        "--repo",
        default=".",
        help="Repo root (used for git + relative paths). Default: .",
    )

    modes = parser.add_mutually_exclusive_group(required=True)
    modes.add_argument(
        "--changed-since",
        metavar="BASE_REF",
        help=(
            "Only count *.go files changed since BASE_REF "
            "(via git merge-base)."
        ),
    )
    modes.add_argument(
        "--staged",
        action="store_true",
        help="Only count staged *.go files.",
    )
    modes.add_argument(
        "--commit",
        metavar="COMMIT",
        help="Only count *.go files in the given commit.",
    )
    modes.add_argument(
        "--since-upstream",
        action="store_true",
        help="Only count *.go files changed vs upstream branch.",
    )
    modes.add_argument(
        "--paths",
        nargs="+",
        help="Count *.go files under these paths (dirs or files).",
    )
    modes.add_argument(
        "--files",
        nargs="+",
        help="Count exactly these *.go files.",
    )
    modes.add_argument(
        "--tree",
        nargs="+",
        help="Print per-package ratios recursively under these paths.",
    )

    tests = parser.add_mutually_exclusive_group()
    tests.add_argument(
        "--include-tests",
        action="store_true",
        help="Include *_test.go files in counts.",
    )
    tests.add_argument(
        "--tests-only",
        action="store_true",
        help="Only count *_test.go files.",
    )

    parser.add_argument(
        "--top",
        type=int,
        default=20,
        help="How many top files to print.",
    )

    args = parser.parse_args()
    repo = Path(args.repo).resolve()

    header_lines: list[str] = []
    files: list[Path] | None = None

    git_modes = [
        ("changed_since", "--changed-since"),
        ("staged", "--staged"),
        ("commit", "--commit"),
        ("since_upstream", "--since-upstream"),
    ]
    active_git_mode = next(
        (
            mode_name
            for arg, mode_name in git_modes
            if getattr(args, arg, None)
        ),
        None,
    )
    if active_git_mode and not is_git_repo(str(repo)):
        raise SystemExit(f"{active_git_mode} requires a git repo: {repo}")

    try:
        if args.changed_since:

            merge_base, files = go_files_changed(
                repo,
                args.changed_since,
                include_tests=args.include_tests,
                tests_only=args.tests_only,
            )
            header_lines = [
                f"Mode: changed since {args.changed_since}",
                f"Merge-base: {merge_base}",
            ]
        elif args.staged:
            files = go_files_staged(
                repo,
                include_tests=args.include_tests,
                tests_only=args.tests_only,
            )
            header_lines = ["Mode: staged files"]
        elif args.commit:
            files = go_files_in_commit(
                repo,
                args.commit,
                include_tests=args.include_tests,
                tests_only=args.tests_only,
            )
            header_lines = [f"Mode: commit files: {args.commit}"]
        elif args.since_upstream:
            upstream = upstream_ref(repo)
            ref_range, files = go_files_diff(
                repo,
                upstream,
                "HEAD",
                include_tests=args.include_tests,
                tests_only=args.tests_only,
            )
            header_lines = [f"Mode: changed vs upstream ({ref_range})"]
        elif args.paths:
            files = go_files_under(
                args.paths,
                include_tests=args.include_tests,
                tests_only=args.tests_only,
            )
            header_lines = [f"Mode: under paths: {args.paths}"]
        elif args.files:
            files = [
                p.resolve()
                for f in args.files
                if include_go_file(
                    (p := Path(f)),
                    include_tests=args.include_tests,
                    tests_only=args.tests_only,
                )
            ]
            header_lines = ["Mode: explicit files"]
        elif args.tree:
            stats, total_comment, total_code = package_stats_tree(
                args.tree,
                repo_root=repo,
                include_tests=args.include_tests,
                tests_only=args.tests_only,
            )
            ratio_total = 0.0 if total_code == 0 else total_comment / total_code

            print(f"Repo: {repo}")
            print(f"Mode: package tree under paths: {args.tree}")
            print()
            print(f"Packages: {len(stats)}")
            print(f"Comment-only lines: {total_comment}")
            print(f"Non-comment non-empty lines: {total_code}")
            print(
                "Ratio (comment-only / code): "
                f"{ratio_total:.4f} ({ratio_total * 100:.1f}%)"
            )
            print()
            print_tree(stats)
            return
    except subprocess.CalledProcessError as exc:
        raise SystemExit(_format_git_error(exc)) from exc

    if files is not None:
        print(f"Repo: {repo}")
        for line in header_lines:
            print(line)
        print()
        summarize(files, repo_root=repo, top=args.top)
        return


if __name__ == "__main__":
    main()

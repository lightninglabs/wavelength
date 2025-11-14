#!/bin/bash

# This script checks that a specific Go version is used in all files matching
# a pattern. It's useful to ensure consistency across Docker files, YAML
# configs, and other places where Go version is specified.
#
# Usage: check-go-version.sh <version> <file_pattern> <search_pattern>
# Example: check-go-version.sh 1.25.3 "Dockerfile" "FROM golang:"
# Example: check-go-version.sh 1.25.3 "*.yml *.yaml" "go-version:|GO_VERSION:|go:"

set -euo pipefail

TARGET_VERSION="$1"
FILE_PATTERN="$2"
SEARCH_PATTERN="$3"

if [ -z "$TARGET_VERSION" ] || [ -z "$FILE_PATTERN" ] || [ -z "$SEARCH_PATTERN" ]; then
    echo "Usage: $0 <version> <file_pattern> <search_pattern>"
    echo "Example: $0 1.25.3 'Dockerfile' 'FROM golang:'"
    exit 1
fi

# Find all files matching the pattern (supports multiple patterns space-separated).
if [[ "$FILE_PATTERN" == *" "* ]]; then
    # Multiple patterns - build find arguments array safely.
    PATTERNS=($FILE_PATTERN)
    find_args=()
    for i in "${!PATTERNS[@]}"; do
        if [ ${#find_args[@]} -gt 0 ]; then
            find_args+=(-o)
        fi
        find_args+=(-name "${PATTERNS[$i]}")
    done

    if [ ${#find_args[@]} -gt 0 ]; then
        FILES=$(find . -type f \( "${find_args[@]}" \) -not -path "./vendor/*" -not -path "./.git/*")
    else
        FILES=""
    fi
else
    # Single pattern.
    FILES=$(find . -type f -name "$FILE_PATTERN" -not -path "./vendor/*" -not -path "./.git/*")
fi

if [ -z "$FILES" ]; then
    echo "No files found matching pattern: $FILE_PATTERN"
    exit 0
fi

echo "Checking Go version $TARGET_VERSION in files matching: $FILE_PATTERN"
echo "Search pattern: $SEARCH_PATTERN"
echo ""

ERRORS=0

for file in $FILES; do
    # Check if file contains the search pattern.
    if grep -q -E "$SEARCH_PATTERN" "$file"; then
        # Extract version numbers from the file.
        VERSIONS=$(grep -E "$SEARCH_PATTERN" "$file" | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' || true)

        if [ -z "$VERSIONS" ]; then
            echo "⚠️  WARNING: Found search pattern in $file but no version number"
            continue
        fi

        # Check each version found.
        while IFS= read -r version; do
            if [ "$version" != "$TARGET_VERSION" ]; then
                echo "❌ ERROR: $file has Go version $version (expected $TARGET_VERSION)"
                ERRORS=$((ERRORS + 1))
            else
                echo "✅ OK: $file has correct Go version $TARGET_VERSION"
            fi
        done <<< "$VERSIONS"
    fi
done

echo ""
if [ $ERRORS -gt 0 ]; then
    echo "❌ Found $ERRORS version mismatch(es)"
    echo "Please update all files to use Go $TARGET_VERSION"
    exit 1
else
    echo "✅ All files use the correct Go version: $TARGET_VERSION"
    exit 0
fi

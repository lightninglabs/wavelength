#!/usr/bin/env bash

# check-agent-docs-sync.sh verifies that CLAUDE.md and AGENTS.md are
# identical. These files must be kept in sync as they serve as aliases for
# different AI agent naming conventions.

set -e

# Color codes for output.
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

CLAUDE_FILE="CLAUDE.md"
AGENTS_FILE="AGENTS.md"

# Check if both files exist.
if [[ ! -f "$CLAUDE_FILE" ]]; then
	echo -e "${RED}ERROR: $CLAUDE_FILE not found${NC}"
	exit 1
fi

if [[ ! -f "$AGENTS_FILE" ]]; then
	echo -e "${RED}ERROR: $AGENTS_FILE not found${NC}"
	exit 1
fi

# Compare files byte-by-byte.
if cmp -s "$CLAUDE_FILE" "$AGENTS_FILE"; then
	echo -e "${GREEN}✓ $CLAUDE_FILE and $AGENTS_FILE are identical${NC}"
	exit 0
else
	echo -e "${RED}ERROR: $CLAUDE_FILE and $AGENTS_FILE are not identical${NC}"
	echo -e "${YELLOW}"
	echo "These files must be kept in sync. They serve as aliases for"
	echo "different AI agent naming conventions (some projects use"
	echo "CLAUDE.md, others use AGENTS.md)."
	echo ""
	echo "To fix this, ensure both files have identical content:"
	echo "  cp $CLAUDE_FILE $AGENTS_FILE"
	echo "  # OR"
	echo "  cp $AGENTS_FILE $CLAUDE_FILE"
	echo ""
	echo "Differences:"
	echo -e "${NC}"
	diff -u "$CLAUDE_FILE" "$AGENTS_FILE" || true
	exit 1
fi

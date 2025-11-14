#!/usr/bin/env bash

set -e

# Colors for output.
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Script to manage git submodules.
# Usage: ./scripts/submodule-helper.sh [command]
# Commands:
#   init     - Initialize and update all submodules
#   update   - Update submodules to latest remote commits
#   status   - Show detailed status of all submodules
#   check    - Verify submodules are initialized and up-to-date (CI-friendly)
#   sync     - Sync submodule URLs from .gitmodules

print_error() {
    printf "${RED}ERROR: %s${NC}\n" "$1" >&2
}

print_success() {
    printf "${GREEN}%s${NC}\n" "$1"
}

print_warning() {
    printf "${YELLOW}WARNING: %s${NC}\n" "$1"
}

print_info() {
    printf "%s\n" "$1"
}

# Check if we're in a git repository.
check_git_repo() {
    if ! git rev-parse --git-dir > /dev/null 2>&1; then
        print_error "Not in a git repository"
        exit 1
    fi
}

# Check if .gitmodules exists.
check_gitmodules() {
    if [ ! -f .gitmodules ]; then
        print_error "No .gitmodules file found"
        exit 1
    fi
}

# Initialize submodules.
cmd_init() {
    print_info "Initializing submodules..."

    if git submodule init; then
        print_success "Submodules initialized"
    else
        print_error "Failed to initialize submodules"
        exit 1
    fi

    print_info "Updating submodules..."
    if git submodule update --init --recursive; then
        print_success "Submodules updated successfully"
    else
        print_error "Failed to update submodules"
        print_info ""
        print_info "Troubleshooting tips:"
        print_info "- For private submodules, ensure SSH keys are configured:"
        print_info "  https://docs.github.com/en/authentication/connecting-to-github-with-ssh"
        print_info "- Check that you have access to the submodule repository"
        print_info "- Try: git submodule sync && git submodule update --init"
        exit 1
    fi
}

# Update submodules to latest commits from their tracked branches.
cmd_update() {
    print_info "Updating submodules to latest commits..."

    # First sync URLs in case they changed.
    git submodule sync --recursive

    # Update to latest commits.
    if git submodule update --remote --recursive; then
        print_success "Submodules updated to latest commits"

        # Show what changed.
        print_info ""
        print_info "Updated submodules status:"
        git submodule status
    else
        print_error "Failed to update submodules"
        print_info ""
        print_info "Troubleshooting tips:"
        print_info "- Verify network connectivity to git remotes"
        print_info "- For private submodules, check SSH key authentication"
        print_info "- Ensure submodule URLs in .gitmodules are correct"
        print_info "- Try: git submodule foreach git fetch --all"
        exit 1
    fi
}

# Show detailed status of submodules.
cmd_status() {
    print_info "Submodule status:"
    print_info ""

    # Get submodule status with commit info.
    git submodule status

    print_info ""

    # Check each submodule for uncommitted changes.
    git submodule foreach --quiet '
        if [ -n "$(git status --porcelain)" ]; then
            echo "⚠️  Submodule $name has uncommitted changes"
            git status --short
        fi
    '

    # Check if submodules are on a detached HEAD.
    git submodule foreach --quiet '
        branch=$(git symbolic-ref --short -q HEAD || echo "DETACHED")
        if [ "$branch" = "DETACHED" ]; then
            echo "ℹ️  Submodule $name is in detached HEAD state"
        else
            echo "✓ Submodule $name is on branch: $branch"
        fi
    '
}

# Check if submodules are properly initialized and up-to-date.
# This is meant to be used in CI to catch uninitialized or stale submodules.
cmd_check() {
    local exit_code=0

    print_info "Checking submodule status..."

    # Get the submodule status output.
    # The first character indicates the status:
    # '-' = not initialized
    # '+' = checked out commit differs from recorded commit
    # 'U' = merge conflicts
    # ' ' = up to date

    while IFS= read -r line; do
        status_char="${line:0:1}"
        submodule_info="${line:1}"

        case "$status_char" in
            -)
                print_error "Submodule not initialized: $submodule_info"
                exit_code=1
                ;;
            +)
                print_warning "Submodule has different commit checked out: $submodule_info"
                print_warning "Run 'make submodule-update' to update to latest"
                # This is a warning, not an error, as the submodule might be
                # intentionally pinned to a specific commit.
                ;;
            U)
                print_error "Submodule has merge conflicts: $submodule_info"
                exit_code=1
                ;;
            ' ')
                print_success "Submodule OK: $submodule_info"
                ;;
            *)
                print_warning "Unknown submodule status '$status_char': $submodule_info"
                ;;
        esac
    done < <(git submodule status)

    if [ $exit_code -eq 0 ]; then
        print_success "All submodules are properly initialized"
    else
        print_error "Some submodules have issues"
    fi

    return $exit_code
}

# Sync submodule URLs from .gitmodules.
cmd_sync() {
    print_info "Syncing submodule URLs..."

    if git submodule sync --recursive; then
        print_success "Submodule URLs synchronized"
    else
        print_error "Failed to sync submodule URLs"
        exit 1
    fi
}

# Show help message.
cmd_help() {
    cat <<EOF
Git Submodule Helper Script

Usage: $0 [command]

Commands:
    init     Initialize and update all submodules (first-time setup)
    update   Update submodules to latest remote commits
    status   Show detailed status of all submodules
    check    Verify submodules are initialized and up-to-date (CI-friendly)
    sync     Sync submodule URLs from .gitmodules
    help     Show this help message

Examples:
    # First-time setup:
    $0 init

    # Update to latest commits:
    $0 update

    # Check status:
    $0 status

    # CI check:
    $0 check
EOF
}

# Main command dispatcher.
main() {
    check_git_repo
    check_gitmodules

    local command="${1:-help}"

    case "$command" in
        init)
            cmd_init
            ;;
        update)
            cmd_update
            ;;
        status)
            cmd_status
            ;;
        check)
            cmd_check
            ;;
        sync)
            cmd_sync
            ;;
        help|--help|-h)
            cmd_help
            ;;
        *)
            print_error "Unknown command: $command"
            cmd_help
            exit 1
            ;;
    esac
}

main "$@"

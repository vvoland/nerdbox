#!/bin/bash
# Merge GitHub PRs into current branch
#
# Usage: Edit the invocations below, then run this script

set -e

merge_pr() {
    local repo=$1
    local pr=$2
    local comment="$3"
    local pr_branch="pr-${repo//\//-}-$pr"

    echo "=================================================="
    echo "Merging $repo#$pr"
    echo "=================================================="

    # Fetch the PR
    echo "Fetching PR #$pr from $repo..."
    if ! git fetch "git@github.com:$repo.git" "pull/$pr/head:$pr_branch"; then
        echo "Error: Failed to fetch PR #$pr from $repo"
        return 1
    fi

    echo "Merging $repo#$pr ($comment)"
    if ! git merge --no-edit --no-ff "$pr_branch" -m "Merge $repo#$pr ($comment)"; then
        echo ""
        echo "Error: Merge failed!"
        echo "Resolve conflicts, then run:"
        git branch -D "$pr_branch" 2>/dev/null || true
        exit 1
    fi

    echo "✓ Successfully merged $repo#$pr"
    echo ""

    # Cleanup
    git branch -D "$pr_branch" 2>/dev/null || true
}

echo "Current branch: $(git branch --show-current)"
echo ""

# ============================================================================
# Edit this section to add/remove PRs to merge
# ============================================================================

# merge_pr <owner/repo> <pr_number> <short comment>
merge_pr containerd/nerdbox 98 "updated containerd to wip-2.3 branch"
merge_pr containerd/nerdbox 78 "add flag vnet_hdr"
merge_pr containerd/nerdbox 81 "virtiofs support for bind mounts"
merge_pr docker/docker-next-nerdbox 19 "Previously merged commits that need a new PR"
merge_pr docker/docker-next-nerdbox 21 "disable VM debug logs"
merge_pr docker/docker-next-nerdbox 20 "Windows support"
merge_pr docker/docker-next-nerdbox 22 "Add Hyper-V enlightments to kernel"

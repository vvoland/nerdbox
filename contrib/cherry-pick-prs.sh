#!/bin/bash
# Cherry-pick GitHub PRs into current branch
#
# Usage: Edit the invocations below, then run this script

set -e

main_branch="tmp-containerd-nerdbox-main"
# Fetch the main branch of the PR's repository
echo "Fetching main branch from containerd/nerdbox..."
if ! git fetch "git@github.com:containerd/nerdbox.git" "main:$main_branch"; then
    echo "Error: Failed to fetch main branch from containerd/nerdbox"
    return 1
fi

cherry_pick_pr() {
    local repo=$1
    local pr=$2
    local comment="$3"
    local pr_branch="pr-${repo//\//-}-$pr"
    local work_branch="cherry-pick-${repo//\//-}-$pr"

    echo "=================================================="
    echo "Cherry-picking $repo#$pr"
    echo "=================================================="

    local original_branch=$(git branch --show-current)

    # Fetch the PR
    echo "Fetching PR #$pr from $repo..."
    if ! git fetch "git@github.com:$repo.git" "pull/$pr/head:$pr_branch"; then
        echo "Error: Failed to fetch PR #$pr from $repo"
        return 1
    fi

    # Get merge base and commits (using main branch of nerdbox repo)
    local merge_base=$(git merge-base "$main_branch" "$pr_branch")
    local commits=$(git rev-list --reverse "$merge_base..$pr_branch")
    local count=$(echo "$commits" | wc -l)

    echo "Found $count commit(s) to cherry-pick"
    echo ""
    git --no-pager log --oneline "$merge_base..$pr_branch"
    echo ""

    # Create a new branch for cherry-picking
    echo "Creating work branch: $work_branch"
    git checkout -B "$work_branch"

    # Cherry-pick each commit
    for commit in $commits; do
        echo "Cherry-picking: $(git log -1 --oneline $commit)"
        if ! git cherry-pick --empty=drop "$commit"; then
            echo ""
            echo "Error: Cherry-pick failed!"
            echo "Resolve conflicts, then run:"
            echo "  git cherry-pick --continue"
            echo "  git checkout $original_branch"
            echo "  git merge --no-ff $work_branch -m \"Merge $repo#$pr ($comment)\""
            echo "  git branch -D $work_branch"
            echo "  git branch -D $pr_branch"
            exit 1
        fi
    done

    # Switch back to original branch and merge
    echo "Merging $work_branch back to $original_branch..."
    git checkout "$original_branch"
    if ! git merge --no-ff "$work_branch" -m "Merge $repo#$pr ($comment)"; then
        echo ""
        echo "Error: Merge failed!"
        echo "Resolve conflicts, then run:"
        echo "  git commit"
        echo "  git branch -D $work_branch"
        echo "  git branch -D $pr_branch"
        exit 1
    fi

    echo "✓ Successfully cherry-picked $repo#$pr"
    echo ""

    # Cleanup
    git branch -D "$work_branch" 2>/dev/null || true
    git branch -D "$pr_branch" 2>/dev/null || true
}

echo "Current branch: $(git branch --show-current)"
echo ""

# ============================================================================
# Edit this section to add/remove PRs to cherry-pick
# ============================================================================

# cherry_pick_pr <owner/repo> <pr_number>
cherry_pick_pr containerd/nerdbox 98 "updated containerd to wip-2.3 branch"
cherry_pick_pr containerd/nerdbox 78 "add flag vnet_hdr"
cherry_pick_pr containerd/nerdbox 81 "virtiofs support for bind mounts"
cherry_pick_pr docker/docker-next-nerdbox 19 "Previously merged commits that need a new PR"
cherry_pick_pr docker/docker-next-nerdbox 21 "disable VM debug logs"
cherry_pick_pr docker/docker-next-nerdbox 20 "Windows support"
cherry_pick_pr docker/docker-next-nerdbox 22 "Add Hyper-V enlightments to kernel"

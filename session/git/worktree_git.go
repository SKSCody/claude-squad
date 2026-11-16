package git

import (
	"claude-squad/log"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// MaxBranchSearchResults is the maximum number of branches returned by SearchBranches.
const MaxBranchSearchResults = 50

// FetchBranches fetches and prunes remote-tracking branches (best-effort, won't fail if offline).
func FetchBranches(repoPath string) {
	cmd := exec.Command("git", "-C", repoPath, "fetch", "--prune")
	_ = cmd.Run()
}

// SearchBranches searches for branches whose name contains filter (case-insensitive),
// ordered by most recently updated first. Returns at most MaxBranchSearchResults.
// If filter is empty, returns all branches up to the limit.
func SearchBranches(repoPath, filter string) ([]string, error) {
	cmd := exec.Command("git", "-C", repoPath, "branch", "-a",
		"--sort=-committerdate",
		"--format=%(refname:short)")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to list branches: %s (%w)", output, err)
	}

	seen := make(map[string]bool)
	var branches []string
	lower := strings.ToLower(filter)
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, "HEAD") {
			continue
		}
		name := strings.TrimPrefix(line, "origin/")
		if seen[name] {
			continue
		}
		seen[name] = true
		if filter != "" && !strings.Contains(strings.ToLower(name), lower) {
			continue
		}
		branches = append(branches, name)
		if len(branches) >= MaxBranchSearchResults {
			break
		}
	}
	return branches, nil
}

// runGitCommand executes a git command and returns any error
func (g *GitWorktree) runGitCommand(path string, args ...string) (string, error) {
	baseArgs := []string{"-C", path}
	cmd := exec.Command("git", append(baseArgs, args...)...)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git command failed: %s (%w)", output, err)
	}

	return string(output), nil
}

// PushChanges commits and pushes changes in the worktree to the remote branch.
// If remote is empty, the branch is pushed to origin and synced using the GitHub CLI.
// Otherwise the branch is pushed to the given remote with git only.
// When force is true the push uses --force-with-lease, for history that was rewritten
// (see PrepareSquashCommit).
func (g *GitWorktree) PushChanges(commitMessage string, open bool, remote string, force bool) error {
	if remote == "" {
		if err := checkGHCLI(); err != nil {
			return err
		}
	}

	// Check if there are any changes to commit
	isDirty, err := g.IsDirty()
	if err != nil {
		return fmt.Errorf("failed to check for changes: %w", err)
	}

	if isDirty {
		if err := g.StageAll(); err != nil {
			return err
		}

		// Create commit
		if _, err := g.runGitCommand(g.worktreePath, "commit", "-m", commitMessage, "--no-verify"); err != nil {
			log.ErrorLog.Print(err)
			return fmt.Errorf("failed to commit changes: %w", err)
		}
	}

	pushRemote := remote
	if pushRemote == "" {
		pushRemote = "origin"
	}

	// Push the branch to the remote, creating it if it doesn't exist
	pushArgs := []string{"push", "-u"}
	if force {
		pushArgs = append(pushArgs, "--force-with-lease")
	}
	gitPushCmd := exec.Command("git", append(pushArgs, pushRemote, g.branchName)...)
	gitPushCmd.Dir = g.worktreePath
	if pushOutput, pushErr := gitPushCmd.CombinedOutput(); pushErr != nil {
		log.ErrorLog.Print(pushErr)
		return fmt.Errorf("failed to push branch: %s (%w)", pushOutput, pushErr)
	}

	if remote == "" {
		// Now sync with remote
		syncCmd := exec.Command("gh", "repo", "sync", "-b", g.branchName)
		syncCmd.Dir = g.worktreePath
		if output, err := syncCmd.CombinedOutput(); err != nil {
			log.ErrorLog.Print(err)
			return fmt.Errorf("failed to sync changes: %s (%w)", output, err)
		}
	}

	// Open the branch in the browser
	if open {
		if err := g.OpenBranchURL(); err != nil {
			// Just log the error but don't fail the push operation
			log.ErrorLog.Printf("failed to open branch URL: %v", err)
		}
	}

	return nil
}

// StageAll stages all changes in the worktree.
func (g *GitWorktree) StageAll() error {
	if _, err := g.runGitCommand(g.worktreePath, "add", "."); err != nil {
		log.ErrorLog.Print(err)
		return fmt.Errorf("failed to stage changes: %w", err)
	}
	return nil
}

// CommitEditorCmd returns a git commit command for the worktree that opens the
// user's configured git editor to write the commit message, prefilled with
// initialMessage when it is non-empty. It is meant to be run interactively in the
// user's terminal (e.g. via tea.ExecProcess). Git aborts the commit with a non-zero
// exit status if the message is left empty.
func (g *GitWorktree) CommitEditorCmd(initialMessage string) *exec.Cmd {
	args := []string{"commit", "--no-verify"}
	if strings.TrimSpace(initialMessage) != "" {
		args = append(args, "-e", "-m", initialMessage)
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = g.worktreePath
	return cmd
}

// autoCommitPrefix marks commits claude-squad creates on its own (pause, auto-push).
const autoCommitPrefix = "[claudesquad]"

// PrepareSquashCommit stages everything and soft-resets the branch to the session's base
// commit, so that the next commit holds all of the session's changes. It returns the HEAD
// to restore (with RestoreHead) if that commit is cancelled, and the message of the newest
// commit in the session that claude-squad didn't write itself, to prefill the editor.
// hasChanges is false when the session has no changes relative to its base; HEAD is left
// where it was in that case.
func (g *GitWorktree) PrepareSquashCommit() (prevHead string, message string, hasChanges bool, err error) {
	out, err := g.runGitCommand(g.worktreePath, "rev-parse", "HEAD")
	if err != nil {
		return "", "", false, fmt.Errorf("failed to read HEAD: %w", err)
	}
	prevHead = strings.TrimSpace(out)

	if err := g.StageAll(); err != nil {
		return "", "", false, err
	}

	if g.baseCommitSHA != "" {
		out, err := g.runGitCommand(g.worktreePath, "log", "-n1", "--format=%B",
			"--invert-grep", "--grep=^"+regexp.QuoteMeta(autoCommitPrefix),
			g.baseCommitSHA+"..HEAD")
		if err != nil {
			return "", "", false, fmt.Errorf("failed to read session commits: %w", err)
		}
		message = strings.TrimSpace(out)

		if _, err := g.runGitCommand(g.worktreePath, "reset", "--soft", g.baseCommitSHA); err != nil {
			return "", "", false, fmt.Errorf("failed to squash session commits: %w", err)
		}
	}

	// `git diff --cached --quiet` exits 1 when something is staged.
	if _, err := g.runGitCommand(g.worktreePath, "diff", "--cached", "--quiet"); err == nil {
		if err := g.RestoreHead(prevHead); err != nil {
			return "", "", false, err
		}
		return prevHead, message, false, nil
	}
	return prevHead, message, true, nil
}

// RestoreHead moves the branch back to sha without touching the index or working tree,
// undoing PrepareSquashCommit.
func (g *GitWorktree) RestoreHead(sha string) error {
	if _, err := g.runGitCommand(g.worktreePath, "reset", "--soft", sha); err != nil {
		return fmt.Errorf("failed to restore branch to %s: %w", sha, err)
	}
	return nil
}

// CommitChanges commits changes locally without pushing to remote
func (g *GitWorktree) CommitChanges(commitMessage string) error {
	// Check if there are any changes to commit
	isDirty, err := g.IsDirty()
	if err != nil {
		return fmt.Errorf("failed to check for changes: %w", err)
	}

	if isDirty {
		if err := g.StageAll(); err != nil {
			return err
		}

		// Create commit (local only)
		if _, err := g.runGitCommand(g.worktreePath, "commit", "-m", commitMessage, "--no-verify"); err != nil {
			log.ErrorLog.Print(err)
			return fmt.Errorf("failed to commit changes: %w", err)
		}
	}

	return nil
}

// IsDirty checks if the worktree has uncommitted changes
func (g *GitWorktree) IsDirty() (bool, error) {
	output, err := g.runGitCommand(g.worktreePath, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("failed to check worktree status: %w", err)
	}
	return len(output) > 0, nil
}

// IsValidWorktree reports whether the worktree path exists and contains a
// .git entry, i.e. git can still recognize it as a working tree.
// Returns (false, nil) if the worktree is orphaned (path or .git missing).
func (g *GitWorktree) IsValidWorktree() (bool, error) {
	if _, err := os.Stat(g.worktreePath); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to stat worktree path: %w", err)
	}
	if _, err := os.Stat(filepath.Join(g.worktreePath, ".git")); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to stat worktree .git: %w", err)
	}
	return true, nil
}

// IsBranchCheckedOut checks if the instance branch is currently checked out
func (g *GitWorktree) IsBranchCheckedOut() (bool, error) {
	output, err := g.runGitCommand(g.repoPath, "branch", "--show-current")
	if err != nil {
		return false, fmt.Errorf("failed to get current branch: %w", err)
	}
	return strings.TrimSpace(string(output)) == g.branchName, nil
}

// OpenBranchURL opens the branch URL in the default browser
func (g *GitWorktree) OpenBranchURL() error {
	// Check if GitHub CLI is available
	if err := checkGHCLI(); err != nil {
		return err
	}

	cmd := exec.Command("gh", "browse", "--branch", g.branchName)
	cmd.Dir = g.worktreePath
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to open branch URL: %w", err)
	}
	return nil
}

package git

import (
	"claude-squad/log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	log.Initialize(false)
	code := m.Run()
	log.Close()
	os.Exit(code)
}

func TestPushChanges_CustomRemote(t *testing.T) {
	root := t.TempDir()

	remotePath := filepath.Join(root, "fork.git")
	mustRunGit(t, "", "init", "--bare", remotePath)

	repoPath := filepath.Join(root, "repo")
	mustRunGit(t, "", "init", repoPath)
	mustRunGit(t, repoPath, "config", "user.name", "Test User")
	mustRunGit(t, repoPath, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repoPath, "README.md"), []byte("hello\n"), 0644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	mustRunGit(t, repoPath, "add", "README.md")
	mustRunGit(t, repoPath, "commit", "-m", "initial")
	mustRunGit(t, repoPath, "remote", "add", "upstream", remotePath)

	branchName := "Ticket/1/Foo"
	mustRunGit(t, repoPath, "checkout", "-b", branchName)

	// Leave uncommitted work so PushChanges has to commit it first.
	if err := os.WriteFile(filepath.Join(repoPath, "change.txt"), []byte("change\n"), 0644); err != nil {
		t.Fatalf("write change: %v", err)
	}

	g := &GitWorktree{
		repoPath:     repoPath,
		worktreePath: repoPath,
		branchName:   branchName,
	}

	if err := g.PushChanges("test commit", false, "upstream", false); err != nil {
		t.Fatalf("PushChanges() error = %v", err)
	}

	refs := mustRunGit(t, repoPath, "ls-remote", "upstream")
	if !strings.Contains(refs, "refs/heads/"+branchName) {
		t.Fatalf("branch %q not found on remote, ls-remote output:\n%s", branchName, refs)
	}

	if dirty, err := g.IsDirty(); err != nil {
		t.Fatalf("IsDirty() error = %v", err)
	} else if dirty {
		t.Fatalf("worktree still dirty after PushChanges")
	}

	upstreamBranch := strings.TrimSpace(mustRunGit(t, repoPath, "rev-parse", "--abbrev-ref", branchName+"@{upstream}"))
	if upstreamBranch != "upstream/"+branchName {
		t.Fatalf("upstream tracking = %q, want %q", upstreamBranch, "upstream/"+branchName)
	}
}

func TestCommitEditorCmd(t *testing.T) {
	newRepo := func(t *testing.T) *GitWorktree {
		t.Helper()
		repoPath := filepath.Join(t.TempDir(), "repo")
		mustRunGit(t, "", "init", repoPath)
		mustRunGit(t, repoPath, "config", "user.name", "Test User")
		mustRunGit(t, repoPath, "config", "user.email", "test@example.com")
		if err := os.WriteFile(filepath.Join(repoPath, "README.md"), []byte("hello\n"), 0644); err != nil {
			t.Fatalf("write README: %v", err)
		}
		mustRunGit(t, repoPath, "add", "README.md")
		mustRunGit(t, repoPath, "commit", "-m", "initial")
		if err := os.WriteFile(filepath.Join(repoPath, "change.txt"), []byte("change\n"), 0644); err != nil {
			t.Fatalf("write change: %v", err)
		}
		return &GitWorktree{repoPath: repoPath, worktreePath: repoPath, branchName: "main"}
	}

	t.Run("commits with the message written in the editor", func(t *testing.T) {
		g := newRepo(t)
		if err := g.StageAll(); err != nil {
			t.Fatalf("StageAll() error = %v", err)
		}
		staged := mustRunGit(t, g.worktreePath, "diff", "--cached", "--name-only")
		if !strings.Contains(staged, "change.txt") {
			t.Fatalf("change.txt not staged, got:\n%s", staged)
		}

		cmd := g.CommitEditorCmd("")
		if cmd.Dir != g.worktreePath {
			t.Fatalf("cmd.Dir = %q, want %q", cmd.Dir, g.worktreePath)
		}
		// Simulate the user writing a message in their editor.
		cmd.Env = append(os.Environ(), `GIT_EDITOR=sh -c 'printf "custom message\n" > "$1"' editor`)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("commit failed: %v\n%s", err, out)
		}

		subject := strings.TrimSpace(mustRunGit(t, g.worktreePath, "log", "-1", "--format=%s"))
		if subject != "custom message" {
			t.Fatalf("commit subject = %q, want %q", subject, "custom message")
		}
		if dirty, err := g.IsDirty(); err != nil || dirty {
			t.Fatalf("IsDirty() = %v, %v; want false, nil", dirty, err)
		}
	})

	t.Run("empty message aborts the commit", func(t *testing.T) {
		g := newRepo(t)
		if err := g.StageAll(); err != nil {
			t.Fatalf("StageAll() error = %v", err)
		}
		cmd := g.CommitEditorCmd("")
		// Editor that leaves only git's comment template behind.
		cmd.Env = append(os.Environ(), "GIT_EDITOR=true")
		if err := cmd.Run(); err == nil {
			t.Fatalf("expected commit with empty message to fail")
		}
		if dirty, err := g.IsDirty(); err != nil || !dirty {
			t.Fatalf("IsDirty() = %v, %v; want true, nil", dirty, err)
		}
	})
}

func TestCommitEditorCmd_PrefillsMessage(t *testing.T) {
	g := &GitWorktree{worktreePath: "/tmp/wt"}
	cmd := g.CommitEditorCmd("previous message")
	got := strings.Join(cmd.Args, " ")
	if !strings.Contains(got, "-e -m previous message") {
		t.Fatalf("args = %q, want a prefilled -e -m message", got)
	}
}

// newSquashRepo creates a repo on a session branch with a base commit, and returns the
// worktree with its base recorded.
func newSquashRepo(t *testing.T) *GitWorktree {
	t.Helper()
	repoPath := filepath.Join(t.TempDir(), "repo")
	mustRunGit(t, "", "init", repoPath)
	mustRunGit(t, repoPath, "config", "user.name", "Test User")
	mustRunGit(t, repoPath, "config", "user.email", "test@example.com")
	writeFile(t, repoPath, "README.md", "hello\n")
	mustRunGit(t, repoPath, "add", "README.md")
	mustRunGit(t, repoPath, "commit", "-m", "initial")
	mustRunGit(t, repoPath, "checkout", "-b", "session")
	base := strings.TrimSpace(mustRunGit(t, repoPath, "rev-parse", "HEAD"))
	return &GitWorktree{repoPath: repoPath, worktreePath: repoPath, branchName: "session", baseCommitSHA: base}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestPrepareSquashCommit(t *testing.T) {
	t.Run("squashes session commits and uncommitted work", func(t *testing.T) {
		g := newSquashRepo(t)
		writeFile(t, g.worktreePath, "a.txt", "a\n")
		mustRunGit(t, g.worktreePath, "add", ".")
		mustRunGit(t, g.worktreePath, "commit", "-m", "my real message\n\nwith a body")
		writeFile(t, g.worktreePath, "b.txt", "b\n")
		mustRunGit(t, g.worktreePath, "add", ".")
		mustRunGit(t, g.worktreePath, "commit", "-m", "[claudesquad] update from 'x' (paused)")
		writeFile(t, g.worktreePath, "c.txt", "c\n")
		prev := strings.TrimSpace(mustRunGit(t, g.worktreePath, "rev-parse", "HEAD"))

		prevHead, message, hasChanges, err := g.PrepareSquashCommit()
		if err != nil {
			t.Fatalf("PrepareSquashCommit() error = %v", err)
		}
		if !hasChanges {
			t.Fatal("hasChanges = false, want true")
		}
		if prevHead != prev {
			t.Fatalf("prevHead = %q, want %q", prevHead, prev)
		}
		if message != "my real message\n\nwith a body" {
			t.Fatalf("message = %q", message)
		}
		head := strings.TrimSpace(mustRunGit(t, g.worktreePath, "rev-parse", "HEAD"))
		if head != g.baseCommitSHA {
			t.Fatalf("HEAD = %q, want base %q", head, g.baseCommitSHA)
		}
		staged := mustRunGit(t, g.worktreePath, "diff", "--cached", "--name-only")
		for _, f := range []string{"a.txt", "b.txt", "c.txt"} {
			if !strings.Contains(staged, f) {
				t.Fatalf("%s not staged, got:\n%s", f, staged)
			}
		}

		// Aborting restores the original commits; the working tree is untouched.
		if err := g.RestoreHead(prevHead); err != nil {
			t.Fatalf("RestoreHead() error = %v", err)
		}
		head = strings.TrimSpace(mustRunGit(t, g.worktreePath, "rev-parse", "HEAD"))
		if head != prev {
			t.Fatalf("HEAD after restore = %q, want %q", head, prev)
		}
		if _, err := os.Stat(filepath.Join(g.worktreePath, "c.txt")); err != nil {
			t.Fatalf("c.txt missing after restore: %v", err)
		}
	})

	t.Run("only auto commits gives an empty message", func(t *testing.T) {
		g := newSquashRepo(t)
		writeFile(t, g.worktreePath, "a.txt", "a\n")
		mustRunGit(t, g.worktreePath, "add", ".")
		mustRunGit(t, g.worktreePath, "commit", "-m", "[claudesquad] update from 'x' (paused)")

		_, message, hasChanges, err := g.PrepareSquashCommit()
		if err != nil || !hasChanges {
			t.Fatalf("PrepareSquashCommit() = %v, %v; want true, nil", hasChanges, err)
		}
		if message != "" {
			t.Fatalf("message = %q, want empty", message)
		}
	})

	t.Run("no changes leaves HEAD alone", func(t *testing.T) {
		g := newSquashRepo(t)
		prevHead, _, hasChanges, err := g.PrepareSquashCommit()
		if err != nil {
			t.Fatalf("PrepareSquashCommit() error = %v", err)
		}
		if hasChanges {
			t.Fatal("hasChanges = true, want false")
		}
		head := strings.TrimSpace(mustRunGit(t, g.worktreePath, "rev-parse", "HEAD"))
		if head != prevHead || head != g.baseCommitSHA {
			t.Fatalf("HEAD = %q, want %q", head, g.baseCommitSHA)
		}
	})
}

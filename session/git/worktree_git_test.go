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

	if err := g.PushChanges("test commit", false, "upstream"); err != nil {
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

		cmd := g.CommitEditorCmd()
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
		cmd := g.CommitEditorCmd()
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

// Package worktree gives each agent its own git worktree (plan §3.4,
// isolation "worktree"), by shelling out to git (2.38+ for merge-tree).
//
// Layout: the agent's worktree is .tazzyronth/worktrees/<agent>; a task is
// worked on branch tazzy/<task> started from the integration branch, and a
// finished task is merged into the integration branch with merge-tree +
// commit-tree + update-ref, so no checkout (least of all the user's) is
// touched by a merge.
package worktree

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Repo is a project that is the top level of a git repository with at least
// one commit. It is not safe for concurrent use.
type Repo struct {
	root  string
	ident []string // -c user.name/-c user.email when the repo has no identity configured
}

// Open checks that root can use worktree isolation.
func Open(root string) (*Repo, error) {
	r := &Repo{root: root}
	prefix, err := r.git(root, "rev-parse", "--show-prefix")
	if err != nil {
		return nil, errors.New("worktree isolation needs a git repository: " + err.Error())
	}
	if prefix != "" {
		return nil, fmt.Errorf("worktree isolation needs the project to be the repository's top level (it is %s inside it)", prefix)
	}
	if _, err := r.git(root, "rev-parse", "--verify", "-q", "HEAD^{commit}"); err != nil {
		return nil, errors.New("worktree isolation needs a git repository with at least one commit")
	}
	if email, _ := r.git(root, "config", "user.email"); email == "" {
		r.ident = []string{"-c", "user.name=Tazzyronth", "-c", "user.email=tazzy@localhost"}
	}
	return r, nil
}

// Branch is the branch a task is worked on.
func Branch(task string) string {
	b := []byte(task)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			b[i] = '-'
		}
	}
	return "tazzy/" + string(b)
}

func (r *Repo) base() string { return filepath.Join(r.root, ".tazzyronth", "worktrees") }

// Dir is where agent's worktree lives (whether or not it exists).
func (r *Repo) Dir(agent string) string { return Dir(r.root, agent) }

// Dir is where agent's worktree in the project at root lives.
func Dir(root, agent string) string { return filepath.Join(root, ".tazzyronth", "worktrees", agent) }

// On reports whether agent's worktree is on task's branch.
func (r *Repo) On(agent, task string) bool {
	cur, _ := r.git(r.Dir(agent), "symbolic-ref", "-q", "--short", "HEAD")
	return cur == Branch(task)
}

// Exists reports whether dir is a git worktree.
func Exists(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

func (r *Repo) git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append(append([]string{"-c", "core.quotepath=off"}, r.ident...), args...)...)
	cmd.Dir = dir
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(out.String()), fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// NewIntegration creates branch name at the current HEAD.
func (r *Repo) NewIntegration(name string) error {
	_, err := r.git(r.root, "branch", name, "HEAD")
	return err
}

// Prune drops git's records of worktrees whose directories are gone.
func (r *Repo) Prune() { r.git(r.root, "worktree", "prune") }

// Ensure creates agent's worktree (detached at integ) if it doesn't exist.
func (r *Repo) Ensure(agent, integ string) (string, error) {
	dir := r.Dir(agent)
	if Exists(dir) {
		return dir, nil
	}
	if err := os.MkdirAll(r.base(), 0o700); err != nil {
		return "", err
	}
	// Keep the worktrees out of the user's `git status` / `git add -A`.
	os.WriteFile(filepath.Join(r.base(), ".gitignore"), []byte("*\n"), 0o600)
	r.Prune() // a stale record of the same path would make add fail
	_, err := r.git(r.root, "worktree", "add", "-q", "--detach", dir, integ)
	return dir, err
}

// Start points agent's worktree at a fresh branch for task from integ,
// discarding anything left in it. If merge is set (a conflict-resolution
// task), that branch is merged in without committing, leaving the conflict
// markers for the agent to resolve.
func (r *Repo) Start(agent, task, integ, merge string) (string, error) {
	dir, err := r.Ensure(agent, integ)
	if err != nil {
		return "", err
	}
	r.git(dir, "merge", "--abort") // a leftover merge blocks the checkout; usually there is none
	if _, err := r.git(dir, "checkout", "-q", "-f", "-B", Branch(task), integ); err != nil {
		return "", err
	}
	if _, err := r.git(dir, "clean", "-q", "-fd"); err != nil {
		return "", err
	}
	if merge != "" {
		// Exit status 1 means "conflicts": exactly what the task is for.
		if _, err := r.git(dir, "merge", "-q", "--no-ff", "--no-commit", merge); err != nil && !r.merging(dir) {
			return "", err
		}
	}
	return dir, nil
}

func (r *Repo) merging(dir string) bool {
	_, err := r.git(dir, "rev-parse", "-q", "--verify", "MERGE_HEAD")
	return err == nil
}

// commit commits everything in dir (outside .tazzyronth), if there is
// anything to commit or a merge to conclude. It refuses to commit unresolved
// conflict markers unless force is set.
func (r *Repo) commit(dir, msg string, force bool) error {
	merging := r.merging(dir)
	if merging && !force {
		out, _ := r.git(dir, "diff", "--name-only", "--diff-filter=U")
		var bad []string
		for _, f := range lines(out) {
			b, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f)))
			if bytes.Contains(b, []byte("<<<<<<< ")) || bytes.Contains(b, []byte(">>>>>>> ")) {
				bad = append(bad, f)
			}
		}
		if bad != nil {
			return fmt.Errorf("unresolved conflict markers in %s: resolve them, then call tazzy_done again", strings.Join(bad, ", "))
		}
	}
	if _, err := r.git(dir, "add", "-A", "--", ".", ":(exclude).tazzyronth"); err != nil {
		return err
	}
	if !merging {
		if _, err := r.git(dir, "diff", "--cached", "--quiet"); err == nil {
			return nil // nothing staged
		}
	}
	_, err := r.git(dir, "commit", "-q", "-m", msg)
	return err
}

// Finish commits agent's work on task (if the agent didn't) and merges its
// branch into integ. On a merge conflict nothing is merged, the task branch
// is kept and the conflicted files are returned; otherwise the task branch
// is deleted. Either way the worktree is left detached at integ's new head,
// and merge (the branch a resolution task merged in, now part of the task
// branch) is deleted.
func (r *Repo) Finish(agent, task, integ, merge, msg string) (conflicts []string, err error) {
	dir, branch := r.Dir(agent), Branch(task)
	if cur, _ := r.git(dir, "symbolic-ref", "-q", "--short", "HEAD"); cur != branch {
		return nil, fmt.Errorf("worktree %s is on %q, not %s", dir, cur, branch)
	}
	if err := r.commit(dir, msg, false); err != nil {
		return nil, err
	}
	if _, err := r.git(r.root, "merge-base", "--is-ancestor", branch, integ); err != nil {
		if conflicts, err = r.merge(integ, branch, msg); err != nil {
			return nil, err
		}
	}
	if _, err := r.git(dir, "checkout", "-q", "--detach", integ); err != nil {
		return conflicts, err
	}
	if merge != "" {
		r.git(r.root, "branch", "-D", merge)
	}
	if conflicts == nil {
		r.git(r.root, "branch", "-D", branch)
	}
	return conflicts, nil
}

// merge merges branch into integ without a checkout.
func (r *Repo) merge(integ, branch, msg string) ([]string, error) {
	old, err := r.git(r.root, "rev-parse", "refs/heads/"+integ)
	if err != nil {
		return nil, err
	}
	out, err := r.git(r.root, "merge-tree", "--write-tree", "--name-only", "--no-messages", integ, branch)
	f := lines(out)
	var ee *exec.ExitError
	switch {
	case err == nil && len(f) > 0:
	case errors.As(err, &ee) && ee.ExitCode() == 1 && len(f) > 1:
		return slices.Compact(slices.Sorted(slices.Values(f[1:]))), nil
	default:
		return nil, fmt.Errorf("merge %s: %v", branch, err)
	}
	c, err := r.git(r.root, "commit-tree", f[0], "-p", integ, "-p", branch, "-m", "tazzy: merge "+branch+"\n\n"+msg)
	if err != nil {
		return nil, err
	}
	_, err = r.git(r.root, "update-ref", "-m", "tazzy: merge "+branch, "refs/heads/"+integ, c, old)
	return nil, err
}

// Abandon ends an unfinished attempt at task: whatever the agent did is
// committed and its branch renamed tazzy/<task>-failed-<attempt>, for
// inspection, so a retry starts clean. The worktree is left detached.
func (r *Repo) Abandon(agent, task string, attempt int) error {
	dir, branch := r.Dir(agent), Branch(task)
	if !Exists(dir) {
		return nil
	}
	if cur, _ := r.git(dir, "symbolic-ref", "-q", "--short", "HEAD"); cur != branch {
		return nil
	}
	err := r.commit(dir, fmt.Sprintf("tazzy: unfinished attempt %d at %s by %s", attempt, task, agent), true)
	if _, e := r.git(dir, "checkout", "-q", "-f", "--detach"); e != nil {
		return errors.Join(err, e)
	}
	_, e := r.git(r.root, "branch", "-M", branch, fmt.Sprintf("%s-failed-%d", branch, attempt))
	return errors.Join(err, e)
}

// Remove deletes agent's worktree (not its branches). On Windows a process
// that just exited may still hold files in it for a moment, so it retries.
func (r *Repo) Remove(agent string) error {
	dir := r.Dir(agent)
	if _, err := os.Stat(dir); err != nil {
		return nil
	}
	var err error
	for range 20 {
		if _, err = r.git(r.root, "worktree", "remove", "--force", "--force", dir); err == nil {
			return nil
		}
		if e := os.RemoveAll(dir); e == nil {
			r.Prune()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return err
}

func lines(s string) []string {
	var out []string
	for l := range strings.Lines(s) {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// Agents lists the agents that have a worktree directory.
func (r *Repo) Agents() []string {
	ents, _ := os.ReadDir(r.base())
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

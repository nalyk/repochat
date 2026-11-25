package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nalyk/repochat/internal/guidance"
	"github.com/nalyk/repochat/internal/repo"
)

// openAssignments opens the guidance store, or returns (nil, nil) when no
// guidance state exists at all, so a machine that never used guidance is
// left exactly as it was: nothing is created and nothing is captured.
func openAssignments(repoRoot string) (*guidance.Store, error) {
	sd, err := stateDir()
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(filepath.Join(sd, "guidance")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return guidance.Open(sd, repoRoot)
}

func assignedElsewhere(root string) error {
	return fmt.Errorf("this tunnel carries owner guidance for %s; use another tunnel, or `repochat trust --release-tunnel` there", repo.SanitizeLine(root))
}

var errRepoChanged = errors.New("the repository changed since its guidance was reviewed; run repochat trust again")

// checkTunnelAssignment refuses to serve r on a tunnel that carries owner
// guidance for another repository, or for this one after it changed. It
// returns nil, without capturing anything, when the tunnel has no assignment.
func checkTunnelAssignment(ctx context.Context, r *repo.Repo, tunnelID string) error {
	store, err := openAssignments(r.Root)
	if err != nil {
		return err
	}
	if store == nil {
		return nil
	}
	key := guidance.Key(tunnelID)
	a, err := store.Assignment(key)
	if err != nil {
		return err
	}
	if a == nil {
		return nil
	}
	b, err := r.CaptureBinding(ctx)
	if err != nil {
		return fmt.Errorf("this tunnel carries owner guidance for %s, and this repository cannot be checked against it: %v", repo.SanitizeLine(a.Root), err)
	}
	if b.Root != a.Root {
		return assignedElsewhere(a.Root)
	}
	if !a.Equal(b) {
		return errRepoChanged
	}
	rec, err := store.Load(key)
	if err != nil {
		return err
	}
	if rec != nil && !rec.Binding.Equal(b) {
		return errRepoChanged
	}
	return nil
}

// checkTunnelUnassigned refuses to unbind a tunnel that carries guidance
// (root == ""), or to bind it to a repository other than its own.
func checkTunnelUnassigned(tunnelID, root string) error {
	store, err := openAssignments("")
	if err != nil || store == nil {
		return err
	}
	a, err := store.Assignment(guidance.Key(tunnelID))
	if err != nil || a == nil {
		return err
	}
	if root != "" {
		if resolved, err := filepath.EvalSymlinks(root); err == nil && resolved == a.Root {
			return nil
		}
	}
	return assignedElsewhere(a.Root)
}

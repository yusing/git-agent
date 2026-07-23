package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/yusing/git-agent/internal/checks"
	checkbuiltin "github.com/yusing/git-agent/internal/checks/builtin"
	"github.com/yusing/git-agent/internal/gitctx"
	reviewtask "github.com/yusing/git-agent/internal/tasks/review"
)

type reviewCheckSession struct {
	scope      checks.Scope
	prepared   *checks.PreparedSet
	verify     func() error
	tempRoot   string
	closed     bool
	checkClose error
}

func newReviewCheckSession(
	repo *gitctx.Repository,
	mode reviewtask.Mode,
	prepared reviewtask.PreparedContext,
) (*reviewCheckSession, error) {
	session := &reviewCheckSession{}
	var scope checks.Scope
	var err error
	switch mode {
	case reviewtask.ModeUncommitted:
		if err := repo.CheckUncommittedFingerprint(prepared.Fingerprint); err != nil {
			return nil, err
		}
		scope, err = checks.NewChangedScope(repo.RootPath, prepared.Paths, prepared.Components)
		session.verify = func() error {
			return repo.CheckUncommittedFingerprint(prepared.Fingerprint)
		}
	case reviewtask.ModeStaged:
		if err := repo.CheckStagedReviewFingerprint(prepared.Fingerprint); err != nil {
			return nil, err
		}
		session.tempRoot, err = os.MkdirTemp("", "git-agent-staged-review-*")
		if err != nil {
			return nil, fmt.Errorf("create staged checker workspace: %w", err)
		}
		if materializeErr := repo.MaterializeStagedReview(session.tempRoot); materializeErr != nil {
			return nil, errors.Join(materializeErr, session.Close())
		}
		if fingerprintErr := repo.CheckStagedReviewFingerprint(prepared.Fingerprint); fingerprintErr != nil {
			return nil, errors.Join(fingerprintErr, session.Close())
		}
		scope, err = checks.NewChangedScope(session.tempRoot, prepared.Paths, prepared.Components)
		session.verify = func() error {
			return repo.CheckStagedReviewFingerprint(prepared.Fingerprint)
		}
	case reviewtask.ModeCodebase:
		scope, err = checks.NewCodebaseScope(repo.RootPath, prepared.Components)
		session.verify = func() error { return nil }
	default:
		return nil, fmt.Errorf("unknown review mode %q", mode)
	}
	if err != nil {
		return nil, errors.Join(err, session.Close())
	}
	session.scope = scope

	set, err := checkbuiltin.New()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("construct bundled checker set: %w", err), session.Close())
	}
	session.prepared, err = set.Prepare(scope)
	if err != nil {
		return nil, errors.Join(err, session.Close())
	}
	return session, nil
}

func (s *reviewCheckSession) Run(
	ctx context.Context,
	executable string,
	progress checks.Progress,
) ([]checks.Result, error) {
	results, runErr := s.prepared.Run(ctx, executable, progress)
	verifyErr := s.verify()
	closeErr := s.Close()
	return results, errors.Join(runErr, verifyErr, closeErr)
}

func (s *reviewCheckSession) SyntheticResults() ([]checks.Result, error) {
	results, resultErr := s.prepared.SyntheticResults(func(name string) []checks.Diagnostic {
		path := "dry-run.go"
		if s.scope.Kind() == checks.ScopeChanged {
			for _, candidate := range s.scope.Paths() {
				if filepath.Ext(candidate) == ".go" {
					path = candidate
					break
				}
			}
		}
		return []checks.Diagnostic{{
			Path: path, Line: 1, Code: "dry-run", Message: name + " deterministic dry-run diagnostic",
		}}
	})
	verifyErr := s.verify()
	closeErr := s.Close()
	return results, errors.Join(resultErr, verifyErr, closeErr)
}

func (s *reviewCheckSession) Close() error {
	if s == nil || s.closed {
		return s.checkClose
	}
	s.closed = true
	if s.tempRoot != "" {
		s.checkClose = os.RemoveAll(s.tempRoot)
		if s.checkClose != nil {
			s.checkClose = fmt.Errorf("remove staged checker workspace: %w", s.checkClose)
		}
	}
	return s.checkClose
}

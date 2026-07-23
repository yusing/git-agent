package builtin

import (
	"github.com/yusing/git-agent/internal/checks"
	"github.com/yusing/git-agent/internal/checks/golangci"
)

func New() (*checks.Set, error) {
	return checks.NewSet(
		golangci.New(),
	)
}

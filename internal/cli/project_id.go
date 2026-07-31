package cli

import (
	"errors"
	"fmt"

	"github.com/yusing/git-agent/internal/projectidentity"
)

func (a *App) runProjectID(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: git-agent project_id")
	}
	identity, err := projectidentity.Resolve(".")
	if err != nil {
		return err
	}
	projectID, err := identity.ID()
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(a.stdout, projectID)
	return err
}

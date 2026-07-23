package builtin

import (
	"testing"

	"github.com/yusing/git-agent/internal/checks/golangci"
)

func TestNewRegistersBundledCheckers(t *testing.T) {
	set, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if set == nil {
		t.Fatal("bundled checker set is nil")
	}
	if err := set.DispatchHelper([]string{"future-checker"}); err == nil {
		t.Fatal("unknown future checker helper accepted")
	}
	if err := set.DispatchHelper([]string{golangci.Name}); err == nil {
		t.Fatal("malformed golangci helper request accepted")
	}
}

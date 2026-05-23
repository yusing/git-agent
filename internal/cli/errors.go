package cli

import "fmt"

type notImplementedError struct {
	feature string
}

func (e notImplementedError) Error() string {
	return fmt.Sprintf("%s: not implemented yet; see docs/spec.md", e.feature)
}

func ErrNotImplemented(feature string) error {
	return notImplementedError{feature: feature}
}

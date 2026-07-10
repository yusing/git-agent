//go:build windows

package search

// Windows exposes no portable directory-sync operation through os.File.
// Snapshot reads use immutable payload offsets and do not depend on catalogs.
func syncDirectory(string) error {
	return nil
}

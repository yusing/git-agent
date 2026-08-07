package gitctx

import (
	"path/filepath"
	"testing"
)

func TestReviewIgnoreMatchersKeepRepositoryRulesLocal(t *testing.T) {
	subSource := initTempRepo(t)
	writeFile(t, filepath.Join(subSource, ".gitignore"), "sub-local-ignore/\n")
	writeFile(t, filepath.Join(subSource, "tracked.txt"), "tracked\n")
	runGit(t, subSource, "add", ".")
	runGit(t, subSource, "commit", "-m", "submodule base")

	root := initTempRepo(t)
	writeFile(t, filepath.Join(root, ".gitignore"), "parent-local-ignore/\n")
	runGit(t, root, "add", ".gitignore")
	runGit(t, root, "commit", "-m", "root base")
	runGit(t, root, "-c", "protocol.file.allow=always", "submodule", "add", subSource, "sub")
	runGit(t, root, "commit", "-m", "add submodule")
	writeFile(t, filepath.Join(root, ".git", "modules", "sub", "info", "exclude"), "sub-info-ignore/\n")

	repo, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	matchers, err := repo.ReviewIgnoreMatchers()
	if err != nil {
		t.Fatal(err)
	}
	if len(matchers) != 2 {
		t.Fatalf("ignore matcher components = %v", mapKeys(matchers))
	}
	rootMatcher, rootFound := matchers[""]
	subMatcher, subFound := matchers["sub"]
	if !rootFound || !subFound {
		t.Fatalf("ignore matcher components = %v", mapKeys(matchers))
	}
	if !rootMatcher.Match([]string{"parent-local-ignore"}, true) {
		t.Fatal("root matcher lost root ignore rule")
	}
	if rootMatcher.Match([]string{"sub-local-ignore"}, true) {
		t.Fatal("root matcher absorbed submodule ignore rule")
	}
	if subMatcher.Match([]string{"parent-local-ignore"}, true) {
		t.Fatal("submodule matcher inherited superproject ignore rule")
	}
	if !subMatcher.Match([]string{"sub-local-ignore"}, true) {
		t.Fatal("submodule matcher lost its .gitignore rule")
	}
	if !subMatcher.Match([]string{"sub-info-ignore"}, true) {
		t.Fatal("submodule matcher lost its info/exclude rule")
	}
}

func mapKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

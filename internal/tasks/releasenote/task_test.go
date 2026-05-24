package releasenote

import (
	"strings"
	"testing"
)

func TestValidateRejectsNonJSON(t *testing.T) {
	t.Parallel()

	errs := Validate("### Breaking Changes\n- nope")
	if len(errs) == 0 {
		t.Fatal("expected non-json validation error")
	}
}

func TestValidateRejectsDuplicateHeading(t *testing.T) {
	t.Parallel()

	errs := Validate(`{
  "sections": [
    {"heading":"Breaking Changes","bullets":[{"summary":"Drop path_patterns","refs":[{"type":"commit","value":"deadbeef"}]}]},
    {"heading":"Breaking Changes","bullets":[{"summary":"Another change","refs":[{"type":"issue","value":"123"}]}]}
  ]
}`)
	if len(errs) == 0 {
		t.Fatal("expected duplicate heading error")
	}
}

func TestValidateRejectsMissingRefs(t *testing.T) {
	t.Parallel()

	errs := Validate(`{
  "sections": [
    {"heading":"Improvements","bullets":[{"summary":"Faster docs refresh","refs":[]}]}
  ]
}`)
	if len(errs) == 0 {
		t.Fatal("expected missing refs error")
	}
}

func TestValidateRejectsInvalidRef(t *testing.T) {
	t.Parallel()

	errs := Validate(`{
  "sections": [
    {"heading":"Bug Fixes","bullets":[{"summary":"Fix middleware order","refs":[{"type":"commit","value":"xyz"}]}]}
  ]
}`)
	if len(errs) == 0 {
		t.Fatal("expected invalid commit ref error")
	}
}

func TestValidateRejectsEmptyChildBullet(t *testing.T) {
	t.Parallel()

	errs := Validate(`{
  "sections": [
    {
      "heading":"Bug Fixes",
      "bullets":[
        {
          "label":"Core/Middleware",
          "summary":"Fix middleware ordering",
          "refs":[{"type":"commit","value":"deadbeef"}],
          "children":[{"summary":""}]
        }
      ]
    }
  ]
}`)
	if len(errs) == 0 {
		t.Fatal("expected empty child bullet error")
	}
}

func TestBuildDocumentSortsSectionsAndAttachesChangelog(t *testing.T) {
	t.Parallel()

	raw := `{
  "sections": [
    {
      "heading": "Improvements",
      "bullets": [
        {
          "summary": "Refresh operator docs",
          "refs": [{"type":"commit","value":"cafebabecafebabecafebabecafebabecafebabe"}]
        }
      ]
    },
    {
      "heading": "Breaking Changes",
      "bullets": [
        {
          "summary": "Remove path_patterns from route config",
          "refs": [{"type":"commit","value":"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"}]
        }
      ]
    }
  ]
}`

	doc, err := BuildDocument(raw, PreparedContext{
		RecommendedSections: []string{
			"### Breaking Changes",
			"### Bug Fixes",
			"### Improvements",
			"### Full Changelog",
		},
		ParentCommits: []PreparedCommit{
			{
				SHA:     "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
				Summary: "refactor(route): remove path_patterns",
				URL:     "https://github.com/example/godoxy/commit/deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
			},
		},
		Submodules: []PreparedSubmodule{
			{
				Path:                  "webui",
				GroupHeading:          "[**webui**](https://github.com/example/webui)",
				LocalHistoryAvailable: true,
				Commits: []PreparedCommit{
					{
						SHA:     "cafebabecafebabecafebabecafebabecafebabe",
						Summary: "docs: refresh routing reference",
						URL:     "https://github.com/example/webui/commit/cafebabecafebabecafebabecafebabecafebabe",
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(doc.Sections) != 2 {
		t.Fatalf("sections = %d", len(doc.Sections))
	}
	if doc.Sections[0].Heading != "Breaking Changes" {
		t.Fatalf("first section = %q", doc.Sections[0].Heading)
	}
	if len(doc.ParentChangelog) != 1 {
		t.Fatalf("parent changelog = %d", len(doc.ParentChangelog))
	}
	if len(doc.Submodules) != 1 || doc.Submodules[0].Path != "webui" {
		t.Fatalf("submodules = %#v", doc.Submodules)
	}
}

func TestRenderBuildsCanonicalMarkdown(t *testing.T) {
	t.Parallel()

	rendered := Render(Document{
		Sections: []Section{
			{
				Heading: "Breaking Changes",
				Bullets: []Bullet{
					{
						Summary: "Remove path_patterns from route config",
						Refs: []Reference{
							{Type: "commit", Value: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"},
							{Type: "issue", Value: "218"},
						},
					},
				},
			},
			{
				Heading: "Bug Fixes",
				Bullets: []Bullet{
					{
						Label:   "Core/Middleware",
						Summary: "Run FileServer middleware after rules settle",
						Refs: []Reference{
							{Type: "pr", Value: "230"},
						},
						Children: []ChildBullet{
							{Summary: "Streaming now works correctly for POST-based SSE endpoints"},
							{Summary: "Large streaming responses no longer buffer unnecessarily"},
						},
					},
				},
			},
		},
		ParentChangelog: []ChangelogEntry{
			{
				SHA:     "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
				Summary: "refactor(route): remove path_patterns",
				URL:     "https://github.com/example/godoxy/commit/deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
			},
		},
		Submodules: []SubmoduleSection{
			{
				Path:    "webui",
				Heading: "[**webui**](https://github.com/example/webui)",
				Entries: []ChangelogEntry{
					{
						SHA:     "cafebabecafebabecafebabecafebabecafebabe",
						Summary: "docs: refresh routing reference",
						URL:     "https://github.com/example/webui/commit/cafebabecafebabecafebabecafebabecafebabe",
					},
				},
			},
		},
	})

	for _, want := range []string{
		"### Breaking Changes",
		"- Remove path_patterns from route config (`deadbee`, #218)",
		"### Bug Fixes",
		"- **Core/Middleware**: Run FileServer middleware after rules settle (#230)",
		"  - Streaming now works correctly for POST-based SSE endpoints",
		"  - Large streaming responses no longer buffer unnecessarily",
		"### Full Changelog",
		"- [`deadbee`](https://github.com/example/godoxy/commit/deadbeefdeadbeefdeadbeefdeadbeefdeadbeef) refactor(route): remove path_patterns",
		"[**webui**](https://github.com/example/webui)",
		"  - [`cafebab`](https://github.com/example/webui/commit/cafebabecafebabecafebabecafebabecafebabe) docs: refresh routing reference",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("render missing %q:\n%s", want, rendered)
		}
	}
}

func TestValidateDocumentRequiresExpectedSubmodules(t *testing.T) {
	t.Parallel()

	errs := ValidateDocument(Document{
		Sections: []Section{
			{
				Heading: "Improvements",
				Bullets: []Bullet{
					{
						Summary: "Refresh docs cache",
						Refs:    []Reference{{Type: "commit", Value: "cafebabecafebabecafebabecafebabecafebabe"}},
					},
				},
			},
		},
		ParentChangelog: []ChangelogEntry{
			{
				SHA:     "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
				Summary: "docs: update notes",
			},
		},
	}, ValidationOptions{
		RequireFullChangelog: true,
		RequiredSubmodules:   []string{"webui"},
	})
	if len(errs) == 0 {
		t.Fatal("expected missing submodule group error")
	}
}

func TestUserPromptContainsSchemaInstructionsAndPreparedContext(t *testing.T) {
	t.Parallel()

	got := UserPrompt(PreparedContext{
		Range:                "v1.0.0..v1.1.0",
		BaseRef:              "v1.0.0",
		BaseSHA:              "base-sha",
		ReleaseRef:           "v1.1.0",
		ReleaseSHA:           "release-sha",
		RepoKind:             "parent-with-submodules",
		ParentRepositoryURL:  "https://github.com/example/godoxy",
		RequireFullChangelog: true,
		RecommendedSections: []string{
			"### Breaking Changes",
			"### Bug Fixes",
			"### Improvements",
			"### Full Changelog",
		},
		ParentCommits: []PreparedCommit{{
			SHA:     "deadbeef",
			Summary: "refactor(route): remove path_patterns",
			URL:     "https://github.com/example/godoxy/commit/deadbeef",
		}},
		RequiredSubmoduleGroups: []string{"webui"},
		Submodules: []PreparedSubmodule{{
			Path:                  "webui",
			BaseSHA:               "webui-base",
			ReleaseSHA:            "webui-release",
			RepositoryURL:         "https://github.com/example/webui",
			GroupHeading:          "[**webui**](https://github.com/example/webui)",
			LocalHistoryAvailable: true,
			Commits: []PreparedCommit{{
				SHA:     "cafebabe",
				Summary: "docs: refresh routing reference",
				URL:     "https://github.com/example/webui/commit/cafebabe",
			}},
		}},
	}, 30, 24)

	for _, want := range []string{
		"Current limits: 30 total model steps, 24 total tool calls.",
		"v1.0.0..v1.1.0",
		`"recommended_sections": [`,
		`"required_submodule_groups": [`,
		`"path": "webui"`,
		`"label"`,
		`"children"`,
		`"refs"`,
		`ref type must be one of: "commit", "pr", "issue"`,
		"only use fallback tools if the prepared context is missing information you need",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("prompt missing %q:\n%s", want, got)
		}
	}
}

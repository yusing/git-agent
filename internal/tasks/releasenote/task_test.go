package releasenote

import (
	"fmt"
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

func TestValidateRejectsChildBullets(t *testing.T) {
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
		t.Fatal("expected child bullet error")
	}
}

func TestValidateAcceptsNullablePracticalOptionalBulletFields(t *testing.T) {
	t.Parallel()

	raw := `{
  "sections": [
    {
      "heading":"Improvements",
      "bullets":[
        {
          "label": null,
          "summary":"Refresh operator docs",
          "refs":[{"type":"commit","value":"cafebabe"}]
        }
      ]
    }
  ]
}`
	if errs := Validate(raw); len(errs) > 0 {
		t.Fatalf("Validate() errors = %v", errs)
	}

	doc, err := BuildDocument(raw, PreparedContext{})
	if err != nil {
		t.Fatal(err)
	}
	rendered := Render(doc)
	if want := "- Refresh operator docs (cafebab)"; !strings.Contains(rendered, want) {
		t.Fatalf("render missing %q:\n%s", want, rendered)
	}
}

func TestOutputSchemaSatisfiesStrictRequiredProperties(t *testing.T) {
	t.Parallel()

	if errs := validateStrictSchemaRequired(OutputSchema(), "$"); len(errs) > 0 {
		t.Fatalf("schema is not strict-compatible:\n%s", strings.Join(errs, "\n"))
	}
}

func TestOutputSchemaDoesNotRequestChildBullets(t *testing.T) {
	t.Parallel()

	if schemaHasProperty(OutputSchema(), "children") {
		t.Fatal("schema should not expose children; release notes should stay flat")
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
	if got := doc.Sections[1].Bullets[0].Refs[0].URL; got != "https://github.com/example/webui/commit/cafebabecafebabecafebabecafebabecafebabe" {
		t.Fatalf("submodule narrative ref URL = %q", got)
	}
}

func TestBuildDocumentRendersSubmoduleNarrativeRefsAsCommitURLs(t *testing.T) {
	t.Parallel()

	raw := `{
  "sections": [
    {
      "heading": "New Features",
      "bullets": [
        {
          "label": "Routing/Rules",
          "summary": "Route rule handling adds do-command option blocks with ordered help, and the WebUI gains matching typing support for option-block and pass/bypass variants",
          "refs": [
            {"type":"commit","value":"344a6db"},
            {"type":"commit","value":"d58bdde"}
          ]
        }
      ]
    }
  ]
}`

	doc, err := BuildDocument(raw, PreparedContext{
		ParentCommits: []PreparedCommit{
			{
				SHA:     "344a6db344a6db344a6db344a6db344a6db344a",
				Summary: "feat(route): add do-command option blocks",
				URL:     "https://github.com/yusing/godoxy/commit/344a6db344a6db344a6db344a6db344a6db344a",
			},
		},
		Submodules: []PreparedSubmodule{
			{
				Path:                  "webui",
				GroupHeading:          "[**webui**](https://github.com/yusing/godoxy-webui)",
				LocalHistoryAvailable: true,
				Commits: []PreparedCommit{
					{
						SHA:     "d58bdde52a992f323e865d5002a3f6dac043068b",
						Summary: "feat(webui): add RuleDo option-block typing and pass/bypass variants",
						URL:     "https://github.com/yusing/godoxy-webui/commit/d58bdde52a992f323e865d5002a3f6dac043068b",
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	rendered := Render(doc)
	want := "- **Routing/Rules**: Route rule handling adds do-command option blocks with ordered help, and the WebUI gains matching typing support for option-block and pass/bypass variants (344a6db, https://github.com/yusing/godoxy-webui/commit/d58bdde52a992f323e865d5002a3f6dac043068b)"
	if !strings.Contains(rendered, want) {
		t.Fatalf("render missing %q:\n%s", want, rendered)
	}
}

func validateStrictSchemaRequired(node any, path string) []string {
	schema, ok := node.(map[string]any)
	if !ok {
		return nil
	}

	var errs []string
	if schemaTypeIncludes(schema["type"], "object") {
		properties, _ := schema["properties"].(map[string]any)
		required, ok := schemaRequiredSet(schema["required"])
		if !ok {
			errs = append(errs, fmt.Sprintf("%s: object schema missing required array", path))
		}
		for name := range properties {
			if !required[name] {
				errs = append(errs, fmt.Sprintf("%s: required missing property %q", path, name))
			}
		}
		for name := range required {
			if _, ok := properties[name]; !ok {
				errs = append(errs, fmt.Sprintf("%s: required has unknown property %q", path, name))
			}
		}
		for name, prop := range properties {
			errs = append(errs, validateStrictSchemaRequired(prop, path+"."+name)...)
		}
	}

	if items, ok := schema["items"]; ok {
		errs = append(errs, validateStrictSchemaRequired(items, path+"[]")...)
	}
	if variants, ok := schema["anyOf"].([]any); ok {
		for i, variant := range variants {
			errs = append(errs, validateStrictSchemaRequired(variant, fmt.Sprintf("%s.anyOf[%d]", path, i))...)
		}
	}

	return errs
}

func schemaTypeIncludes(value any, want string) bool {
	switch v := value.(type) {
	case string:
		return v == want
	case []string:
		for _, typ := range v {
			if typ == want {
				return true
			}
		}
	case []any:
		for _, typ := range v {
			if typ == want {
				return true
			}
		}
	}
	return false
}

func schemaRequiredSet(value any) (map[string]bool, bool) {
	switch required := value.(type) {
	case []string:
		set := make(map[string]bool, len(required))
		for _, name := range required {
			set[name] = true
		}
		return set, true
	case []any:
		set := make(map[string]bool, len(required))
		for _, name := range required {
			text, ok := name.(string)
			if !ok {
				return nil, false
			}
			set[text] = true
		}
		return set, true
	default:
		return nil, false
	}
}

func schemaHasProperty(node any, name string) bool {
	schema, ok := node.(map[string]any)
	if !ok {
		return false
	}
	if properties, ok := schema["properties"].(map[string]any); ok {
		if _, ok := properties[name]; ok {
			return true
		}
		for _, prop := range properties {
			if schemaHasProperty(prop, name) {
				return true
			}
		}
	}
	if items, ok := schema["items"]; ok && schemaHasProperty(items, name) {
		return true
	}
	return false
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
		"- Remove path_patterns from route config (deadbee, #218)",
		"### Bug Fixes",
		"- **Core/Middleware**: Run FileServer middleware after rules settle (#230)",
		"### Full Changelog",
		"- [deadbee](https://github.com/example/godoxy/commit/deadbeefdeadbeefdeadbeefdeadbeefdeadbeef) refactor(route): remove path_patterns",
		"[**webui**](https://github.com/example/webui)",
		"  - [cafebab](https://github.com/example/webui/commit/cafebabecafebabecafebabecafebabecafebabe) docs: refresh routing reference",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("render missing %q:\n%s", want, rendered)
		}
	}
}

func TestRenderOmitsChildBullets(t *testing.T) {
	t.Parallel()

	rendered := Render(Document{
		Sections: []Section{
			{
				Heading: "Bug Fixes",
				Bullets: []Bullet{
					{
						Label:   "Core/Proxy",
						Summary: "Reverse proxy routes now close idle upstream connections when canceled",
						Refs:    []Reference{{Type: "commit", Value: "c4cec0a"}},
						Children: []ChildBullet{
							{Summary: "Helps avoid stale reverse-proxy idle connections lingering after route shutdown"},
							{Summary: "Route shutdown no longer depends on idle upstream cleanup timing"},
						},
					},
				},
			},
		},
	})

	if strings.Contains(rendered, "  - ") {
		t.Fatalf("rendered child bullet:\n%s", rendered)
	}
	if want := "- **Core/Proxy**: Reverse proxy routes now close idle upstream connections when canceled (c4cec0a)"; !strings.Contains(rendered, want) {
		t.Fatalf("render missing %q:\n%s", want, rendered)
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
		`"refs"`,
		`ref type must be one of: "commit", "pr", "issue"`,
		"only use fallback tools if the prepared context is missing information you need",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("prompt missing %q:\n%s", want, got)
		}
	}
}

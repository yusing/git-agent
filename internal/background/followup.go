package background

import (
	"errors"
	"fmt"
	"strings"

	"github.com/yusing/git-agent/internal/followup"
)

// TurnMetadata retains the complete replayable state of one inspection turn.
type TurnMetadata struct {
	ParentID       string `json:"parent_id,omitempty"`
	Mode           string `json:"mode"`
	Workspace      string `json:"workspace,omitempty"`
	ReviewDepth    string `json:"review_depth,omitempty"`
	Depth          int    `json:"depth,omitempty"`
	PromptCacheKey string `json:"prompt_cache_key,omitempty"`
	followup.Tree
}

// AttachTurn adds follow-up metadata to a running task record.
func (s *Store) AttachTurn(id string, metadata TurnMetadata) error {
	return s.withRecordLock(id, func(path string) error {
		record, err := s.readPath(path, id)
		if err != nil {
			return err
		}
		if record.Terminal != nil || record.Turn != nil {
			return fmt.Errorf("background task %s cannot attach turn metadata", id)
		}
		if err := validateTurnMetadata(&metadata, true); err != nil {
			return err
		}
		record.Version = recordVersion
		record.Turn = &metadata
		return s.writeRecord(path, record)
	})
}

// UpdateTurn replaces the preliminary metadata with completed replayable state.
func (s *Store) UpdateTurn(id string, metadata TurnMetadata) error {
	return s.withRecordLock(id, func(path string) error {
		record, err := s.readPath(path, id)
		if err != nil {
			return err
		}
		if record.Terminal != nil || record.Turn == nil {
			return fmt.Errorf("background task %s cannot update turn metadata", id)
		}
		if record.Turn.ParentID != metadata.ParentID ||
			record.Turn.Mode != metadata.Mode ||
			record.Turn.Workspace != metadata.Workspace {
			return fmt.Errorf("background task %s turn identity changed", id)
		}
		if err := validateTurnMetadata(&metadata, true); err != nil {
			return err
		}
		record.Version = recordVersion
		record.Turn = &metadata
		return s.writeRecord(path, record)
	})
}

func validateTurnMetadata(metadata *TurnMetadata, current bool) error {
	if metadata == nil {
		return nil
	}
	if strings.TrimSpace(metadata.Mode) == "" {
		return errors.New("turn metadata requires a mode")
	}
	if metadata.ParentID != "" {
		if err := validateID(metadata.ParentID); err != nil {
			return fmt.Errorf("invalid parent ID: %w", err)
		}
	}
	if !current {
		return nil
	}
	if strings.TrimSpace(metadata.Workspace) == "" {
		return errors.New("turn metadata requires a workspace")
	}
	if strings.TrimSpace(metadata.ReviewDepth) == "" {
		return errors.New("turn metadata requires an inspection depth")
	}
	if metadata.Depth < 0 || metadata.Depth > followup.MaxDepth {
		return fmt.Errorf("turn metadata follow-up depth must be between 0 and %d", followup.MaxDepth)
	}
	if metadata.Depth > 0 && metadata.ParentID == "" {
		return errors.New("positive follow-up depth requires a parent ID")
	}
	if strings.TrimSpace(metadata.PromptCacheKey) == "" {
		return errors.New("turn metadata requires a prompt cache key")
	}
	for index, leaf := range metadata.Leaves {
		if strings.TrimSpace(leaf.Model) == "" {
			return fmt.Errorf("turn metadata leaf %d requires a model", index)
		}
	}
	return nil
}

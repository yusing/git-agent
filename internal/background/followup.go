package background

import (
	"errors"
	"fmt"
	"strings"
)

// TurnMetadata retains the minimum state needed to start a fresh follow-up.
type TurnMetadata struct {
	ParentID string `json:"parent_id,omitempty"`
	Mode     string `json:"mode"`
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
		if err := validateTurnMetadata(&metadata); err != nil {
			return err
		}
		record.Version = recordVersion
		record.Turn = &metadata
		return s.writeRecord(path, record)
	})
}

func validateTurnMetadata(metadata *TurnMetadata) error {
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
	return nil
}

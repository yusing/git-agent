package explore

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"github.com/yusing/git-agent/internal/openai"
)

const (
	defaultBatchSize         = 3
	defaultBatchWait         = 2 * time.Minute
	defaultJoinGrace         = 100 * time.Millisecond
	defaultPollInterval      = 50 * time.Millisecond
	defaultHeartbeatInterval = time.Second
	defaultHeartbeatStale    = 30 * time.Second
	maxBatchFileBytes        = 16 << 20
)

type Prepared struct {
	SemanticResults string
	GuidancePaths   []string
}

type BatchItem struct {
	ID              string   `json:"id"`
	Question        string   `json:"question"`
	SemanticResults string   `json:"semantic_results,omitempty"`
	GuidancePaths   []string `json:"guidance_paths,omitempty"`
}

type BatchResult struct {
	Answer  string
	History []openai.Item
}

type Output struct {
	ID     string `json:"id"`
	Answer string `json:"answer"`
}

type PrepareFunc func(context.Context) (Prepared, error)

type BatchRunner func(context.Context, *Session, []BatchItem) (map[string]BatchResult, error)

// Coordinator synchronizes independent foreground CLI processes through
// owner-only project metadata. It never detaches work from the elected leader.
type Coordinator struct {
	store             *Store
	workspace         string
	workspaceKey      string
	batchSize         int
	batchWait         time.Duration
	joinGrace         time.Duration
	pollInterval      time.Duration
	heartbeatInterval time.Duration
	heartbeatStale    time.Duration
	Progress          func(string)
	DispositionLog    *DispositionLog
}

func NewCoordinator(store *Store, workspace string, fast bool) *Coordinator {
	workspace = filepath.Clean(workspace)
	batchIdentity := workspace
	if fast {
		batchIdentity += "\x00priority"
	}
	sum := sha256.Sum256([]byte(batchIdentity))
	return &Coordinator{
		store:             store,
		workspace:         workspace,
		workspaceKey:      hex.EncodeToString(sum[:]),
		batchSize:         defaultBatchSize,
		batchWait:         defaultBatchWait,
		joinGrace:         defaultJoinGrace,
		pollInterval:      defaultPollInterval,
		heartbeatInterval: defaultHeartbeatInterval,
		heartbeatStale:    defaultHeartbeatStale,
	}
}

type batchState struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

func (c *Coordinator) Run(ctx context.Context, parent *Session, question string, prepare PrepareFunc, runner BatchRunner) (Output, error) {
	if c == nil || c.store == nil {
		return Output{}, errors.New("explore coordinator store is required")
	}
	if runner == nil {
		return Output{}, errors.New("explore batch runner is required")
	}
	question = strings.TrimSpace(question)
	if question == "" {
		return Output{}, errors.New("explore question is empty")
	}
	if parent != nil && parent.Depth >= MaxFollowUps {
		return Output{}, errors.New("context-preserving follow-up limit already exhausted")
	}
	if parent != nil && parent.Workspace != c.workspace {
		return Output{}, fmt.Errorf("explore parent workspace %q does not match current workspace %q", parent.Workspace, c.workspace)
	}
	if parent == nil && prepare == nil {
		return Output{}, errors.New("fresh explore search requires semantic preparation")
	}

	itemID, err := newID()
	if err != nil {
		return Output{}, err
	}
	keyDir, err := c.prepareKeyDir(parent)
	if err != nil {
		return Output{}, err
	}
	intentDir := filepath.Join(keyDir, "intents", itemID)
	if err := os.Mkdir(intentDir, 0o700); err != nil {
		return Output{}, fmt.Errorf("reserve explore batch item: %w", err)
	}
	ownerPath := filepath.Join(keyDir, "owners", itemID)
	stopOwner, err := c.startHeartbeat(ctx, ownerPath)
	if err != nil {
		_ = os.RemoveAll(intentDir)
		return Output{}, err
	}
	defer func() {
		stopOwner()
		_ = os.Remove(ownerPath)
	}()

	record := BatchItem{ID: itemID, Question: question}
	requestPath := filepath.Join(intentDir, "request.json")
	if err := writeJSONAtomic(intentDir, requestPath, record); err != nil {
		_ = os.RemoveAll(intentDir)
		return Output{}, fmt.Errorf("write explore batch reservation: %w", err)
	}
	if prepare != nil {
		c.progress("searching")
		prepared, err := prepare(ctx)
		if err != nil {
			_ = os.RemoveAll(intentDir)
			return Output{}, err
		}
		record.SemanticResults = prepared.SemanticResults
		record.GuidancePaths = prepared.GuidancePaths
		if err := writeJSONAtomic(intentDir, requestPath, record); err != nil {
			_ = os.RemoveAll(intentDir)
			return Output{}, fmt.Errorf("publish explore semantic results: %w", err)
		}
	}
	if err := os.WriteFile(filepath.Join(intentDir, "ready"), nil, 0o600); err != nil {
		_ = os.RemoveAll(intentDir)
		return Output{}, fmt.Errorf("mark explore batch item ready: %w", err)
	}
	if err := sleepContext(ctx, c.joinGrace); err != nil {
		_ = os.RemoveAll(intentDir)
		return Output{}, err
	}

	batchDir, leader, err := c.joinBatch(ctx, keyDir, intentDir, itemID)
	if err != nil {
		_ = os.RemoveAll(intentDir)
		return Output{}, err
	}
	if leader {
		if err := c.runLeader(ctx, keyDir, batchDir, parent, runner); err != nil {
			// The failure is published for every caller by runLeader.
			c.progress("failed")
		}
	} else {
		c.progress("waiting_for_batch")
	}
	return c.waitForAnswer(ctx, keyDir, batchDir, itemID)
}

func (c *Coordinator) prepareKeyDir(parent *Session) (string, error) {
	key := "initial"
	if parent != nil {
		key = parent.ID
	}
	workspaceDir := filepath.Join(c.store.batchDir, c.workspaceKey)
	keyDir := filepath.Join(workspaceDir, key)
	for _, path := range []string{workspaceDir, keyDir, filepath.Join(keyDir, "intents"), filepath.Join(keyDir, "owners")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return "", fmt.Errorf("create explore batch state: %w", err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return "", fmt.Errorf("secure explore batch state: %w", err)
		}
	}
	return keyDir, nil
}

func (c *Coordinator) joinBatch(ctx context.Context, keyDir, intentDir, itemID string) (string, bool, error) {
	var batchDir string
	leader := false
	err := c.withCoordLock(ctx, keyDir, func() error {
		if err := c.sweepLocked(keyDir); err != nil {
			return err
		}
		open, err := c.openBatchLocked(keyDir)
		if err != nil {
			return err
		}
		if open == "" {
			batchID, err := newID()
			if err != nil {
				return err
			}
			open = filepath.Join(keyDir, "batch-"+batchID)
			if err := os.MkdirAll(filepath.Join(open, "items"), 0o700); err != nil {
				return fmt.Errorf("create explore batch: %w", err)
			}
			if err := os.WriteFile(filepath.Join(open, "open"), nil, 0o600); err != nil {
				return fmt.Errorf("open explore batch: %w", err)
			}
			if err := touch(filepath.Join(open, "heartbeat")); err != nil {
				return err
			}
			leader = true
		}
		batchDir = open
		target := filepath.Join(batchDir, "items", itemID)
		if err := os.Rename(intentDir, target); err != nil {
			return fmt.Errorf("join explore batch: %w", err)
		}
		count, err := countDirectories(filepath.Join(batchDir, "items"))
		if err != nil {
			return err
		}
		remaining, err := c.liveIntentCountLocked(keyDir)
		if err != nil {
			return err
		}
		if count >= c.batchSize || (leader && count == 1 && remaining == 0) {
			return sealBatch(batchDir)
		}
		return nil
	})
	return batchDir, leader, err
}

func (c *Coordinator) runLeader(ctx context.Context, keyDir, batchDir string, parent *Session, runner BatchRunner) error {
	stopHeartbeat, err := c.startHeartbeat(ctx, filepath.Join(batchDir, "heartbeat"))
	if err != nil {
		_ = c.failBatch(ctx, keyDir, batchDir, err)
		return err
	}
	defer stopHeartbeat()
	if err := c.collectBatch(ctx, keyDir, batchDir); err != nil {
		_ = c.failBatch(ctx, keyDir, batchDir, err)
		return err
	}
	items, err := readBatchItems(batchDir)
	if err != nil {
		_ = c.failBatch(ctx, keyDir, batchDir, err)
		return err
	}
	if c.DispositionLog != nil {
		_ = c.DispositionLog.AppendBatch(ctx, filepath.Base(batchDir), c.workspace, parent, items)
	}
	c.progress("exploring")
	results, err := runner(ctx, parent, items)
	if err != nil {
		_ = c.failBatch(ctx, keyDir, batchDir, err)
		return err
	}
	for _, item := range items {
		result, ok := results[item.ID]
		if !ok {
			err := fmt.Errorf("explore batch completed without result for %s", item.ID)
			_ = c.failBatch(ctx, keyDir, batchDir, err)
			return err
		}
		depth := 0
		parentID := ""
		if parent != nil {
			depth = parent.Depth + 1
			parentID = parent.ID
		}
		session := Session{
			Version: sessionVersion, ID: item.ID, ParentID: parentID, Depth: depth,
			Workspace: c.workspace, Answer: result.Answer,
			History: append(slices.Clone(result.History), openai.NewMessage(
				"developer",
				"Continue only explore branch item_id "+item.ID+" in future turns; treat sibling items as unrelated context.",
			)),
		}
		if err := c.store.create(session); err != nil {
			_ = c.failBatch(ctx, keyDir, batchDir, err)
			return err
		}
		answerPath := filepath.Join(batchDir, "items", item.ID, "answer.json")
		if err := writeJSONAtomic(filepath.Dir(answerPath), answerPath, Output{ID: item.ID, Answer: result.Answer}); err != nil {
			_ = c.failBatch(ctx, keyDir, batchDir, err)
			return err
		}
	}
	if err := writeJSONAtomic(batchDir, filepath.Join(batchDir, "state.json"), batchState{Status: "ok"}); err != nil {
		_ = c.failBatch(ctx, keyDir, batchDir, err)
		return err
	}
	c.progress("complete")
	return nil
}

func (c *Coordinator) collectBatch(ctx context.Context, keyDir, batchDir string) error {
	if _, err := os.Lstat(filepath.Join(batchDir, "sealed")); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	deadline := time.Now().Add(c.batchWait)
	for {
		sealed := false
		err := c.withCoordLock(ctx, keyDir, func() error {
			if _, err := os.Lstat(filepath.Join(batchDir, "sealed")); err == nil {
				sealed = true
				return nil
			}
			if err := c.pruneDeadIntentsLocked(keyDir); err != nil {
				return err
			}
			count, err := countDirectories(filepath.Join(batchDir, "items"))
			if err != nil {
				return err
			}
			remaining, err := c.liveIntentCountLocked(keyDir)
			if err != nil {
				return err
			}
			if count >= c.batchSize || remaining == 0 || !time.Now().Before(deadline) {
				if err := sealBatch(batchDir); err != nil {
					return err
				}
				sealed = true
			}
			return nil
		})
		if err != nil {
			return err
		}
		if sealed {
			return nil
		}
		if err := sleepContext(ctx, c.pollInterval); err != nil {
			return err
		}
	}
}

func (c *Coordinator) waitForAnswer(ctx context.Context, keyDir, batchDir, itemID string) (Output, error) {
	defer c.consume(ctx, keyDir, batchDir, itemID)
	for {
		statePath := filepath.Join(batchDir, "state.json")
		var state batchState
		if err := readJSONFile(statePath, &state); err == nil {
			switch state.Status {
			case "ok":
				answerPath := filepath.Join(batchDir, "items", itemID, "answer.json")
				var output Output
				if err := readJSONFile(answerPath, &output); err != nil {
					return Output{}, fmt.Errorf("read explore batch answer: %w", err)
				}
				return output, nil
			case "failed":
				if state.Error == "" {
					state.Error = "explore batch failed"
				}
				return Output{}, errors.New(state.Error)
			default:
				return Output{}, fmt.Errorf("invalid explore batch status %q", state.Status)
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return Output{}, err
		}
		if !heartbeatFresh(filepath.Join(batchDir, "heartbeat"), c.heartbeatStale) {
			if err := c.failBatch(ctx, keyDir, batchDir, errors.New("explore batch leader exited before producing answers")); err != nil {
				return Output{}, err
			}
			continue
		}
		if err := sleepContext(ctx, c.pollInterval); err != nil {
			return Output{}, err
		}
	}
}

func (c *Coordinator) failBatch(ctx context.Context, keyDir, batchDir string, cause error) error {
	return c.withCoordLock(context.WithoutCancel(ctx), keyDir, func() error {
		if _, err := os.Lstat(filepath.Join(batchDir, "state.json")); err == nil {
			return nil
		}
		if err := sealBatch(batchDir); err != nil {
			return err
		}
		return writeJSONAtomic(batchDir, filepath.Join(batchDir, "state.json"), batchState{Status: "failed", Error: cause.Error()})
	})
}

func (c *Coordinator) consume(ctx context.Context, keyDir, batchDir, itemID string) {
	itemDir := filepath.Join(batchDir, "items", itemID)
	_ = os.WriteFile(filepath.Join(itemDir, "consumed"), nil, 0o600)
	_ = c.withCoordLock(context.WithoutCancel(ctx), keyDir, func() error {
		inactive, itemIDs := c.batchInactiveLocked(keyDir, batchDir)
		if !inactive {
			return nil
		}
		for _, id := range itemIDs {
			_ = os.Remove(filepath.Join(keyDir, "owners", id))
		}
		return os.RemoveAll(batchDir)
	})
}

func (c *Coordinator) withCoordLock(ctx context.Context, keyDir string, fn func() error) (err error) {
	lockPath := filepath.Join(keyDir, ".coord.lock")
	lock, err := lockCoordinator(ctx, lockPath, c.pollInterval)
	if err != nil {
		return fmt.Errorf("lock explore batch state: %w", err)
	}
	defer func() { err = errors.Join(err, lock.Unlock()) }()
	return fn()
}

func (c *Coordinator) sweepLocked(keyDir string) error {
	if err := c.pruneDeadIntentsLocked(keyDir); err != nil {
		return err
	}
	entries, err := os.ReadDir(keyDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "batch-") {
			continue
		}
		batchDir := filepath.Join(keyDir, entry.Name())
		if _, err := os.Lstat(filepath.Join(batchDir, "state.json")); err == nil {
			inactive, itemIDs := c.batchInactiveLocked(keyDir, batchDir)
			if inactive {
				for _, id := range itemIDs {
					_ = os.Remove(filepath.Join(keyDir, "owners", id))
				}
				if err := os.RemoveAll(batchDir); err != nil {
					return err
				}
			}
			continue
		}
		if !heartbeatFresh(filepath.Join(batchDir, "heartbeat"), c.heartbeatStale) {
			if err := sealBatch(batchDir); err != nil {
				return err
			}
			if err := writeJSONAtomic(batchDir, filepath.Join(batchDir, "state.json"), batchState{
				Status: "failed", Error: "explore batch leader exited before producing answers",
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Coordinator) pruneDeadIntentsLocked(keyDir string) error {
	intentRoot := filepath.Join(keyDir, "intents")
	entries, err := os.ReadDir(intentRoot)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		owner := filepath.Join(keyDir, "owners", entry.Name())
		if heartbeatFresh(owner, c.heartbeatStale) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(intentRoot, entry.Name())); err != nil {
			return err
		}
		_ = os.Remove(owner)
	}
	return nil
}

func (c *Coordinator) openBatchLocked(keyDir string) (string, error) {
	entries, err := os.ReadDir(keyDir)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "batch-") {
			continue
		}
		path := filepath.Join(keyDir, entry.Name())
		if _, err := os.Lstat(filepath.Join(path, "open")); err == nil {
			return path, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}
	return "", nil
}

func (c *Coordinator) liveIntentCountLocked(keyDir string) (int, error) {
	entries, err := os.ReadDir(filepath.Join(keyDir, "intents"))
	if err != nil {
		return 0, err
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() && heartbeatFresh(filepath.Join(keyDir, "owners", entry.Name()), c.heartbeatStale) {
			count++
		}
	}
	return count, nil
}

func (c *Coordinator) batchInactiveLocked(keyDir, batchDir string) (bool, []string) {
	entries, err := os.ReadDir(filepath.Join(batchDir, "items"))
	if err != nil {
		return false, nil
	}
	itemIDs := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		itemIDs = append(itemIDs, entry.Name())
		consumed := filepath.Join(batchDir, "items", entry.Name(), "consumed")
		if _, err := os.Lstat(consumed); err == nil {
			continue
		}
		if heartbeatFresh(filepath.Join(keyDir, "owners", entry.Name()), c.heartbeatStale) {
			return false, itemIDs
		}
	}
	return true, itemIDs
}

func (c *Coordinator) startHeartbeat(parent context.Context, path string) (func(), error) {
	if err := touch(path); err != nil {
		return nil, fmt.Errorf("start explore heartbeat: %w", err)
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	var wg sync.WaitGroup
	wg.Go(func() {
		ticker := time.NewTicker(c.heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = touch(path)
			}
		}
	})
	return func() {
		cancel()
		wg.Wait()
	}, nil
}

func (c *Coordinator) progress(status string) {
	if c.Progress != nil {
		c.Progress(status)
	}
}

func readBatchItems(batchDir string) ([]BatchItem, error) {
	itemRoot := filepath.Join(batchDir, "items")
	entries, err := os.ReadDir(itemRoot)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int { return cmp.Compare(a.Name(), b.Name()) })
	items := make([]BatchItem, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		var record BatchItem
		if err := readJSONFile(filepath.Join(itemRoot, entry.Name(), "request.json"), &record); err != nil {
			return nil, fmt.Errorf("read explore batch item %s: %w", entry.Name(), err)
		}
		if record.ID != entry.Name() {
			return nil, fmt.Errorf("explore batch item ID mismatch for %s", entry.Name())
		}
		items = append(items, record)
	}
	if len(items) == 0 {
		return nil, errors.New("explore batch has no items")
	}
	return items, nil
}

func sealBatch(batchDir string) error {
	if err := os.Remove(filepath.Join(batchDir, "open")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.WriteFile(filepath.Join(batchDir, "sealed"), nil, 0o600)
}

func countDirectories(path string) (int, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() {
			count++
		}
	}
	return count, nil
}

func touch(path string) error {
	now := time.Now()
	if err := os.Chtimes(path, now, now); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	return file.Close()
}

func heartbeatFresh(path string, staleAfter time.Duration) bool {
	info, err := os.Stat(path)
	return err == nil && time.Since(info.ModTime()) < staleAfter
}

func readJSONFile(path string, value any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxBatchFileBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxBatchFileBytes {
		return fmt.Errorf("%s exceeds %d bytes", filepath.Base(path), maxBatchFileBytes)
	}
	if err := sonic.ConfigStd.Unmarshal(data, value); err != nil {
		return fmt.Errorf("decode %s: %w", filepath.Base(path), err)
	}
	return nil
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

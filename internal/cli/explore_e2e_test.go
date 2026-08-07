package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/yusing/git-agent/internal/explore"
)

func TestExploreCommandEndToEndChainResetAndReadTool(t *testing.T) {
	repoDir := newExploreE2ERepository(t)
	executable := buildExploreE2EBinary(t)
	responses, responseStats := newExploreE2EResponsesServer(t)
	defer responses.Close()
	embeddings, embeddingStats := newSearchEmbeddingsServer(t)
	defer embeddings.Close()
	configureExploreE2EEnvironment(t, responses.URL, embeddings.URL)
	projectID := runProjectIDProcess(t, executable, repoDir)

	initial := runSuccessfulExploreProcess(t, executable, repoDir, "read-owner", "of", "Answer")
	if !strings.Contains(initial.output.Answer, "main.go:3") {
		t.Fatalf("initial answer = %q", initial.output.Answer)
	}
	if !strings.Contains(initial.stderr, "explore: searching") || !strings.Contains(initial.stderr, "explore: complete") {
		t.Fatalf("initial stderr missing progress:\n%s", initial.stderr)
	}
	initialEmbeddingCalls := embeddingStats.calls()
	if initialEmbeddingCalls == 0 {
		t.Fatal("initial explore skipped semantic retrieval")
	}

	store, err := explore.NewStore(projectMetadataDir(t, repoDir))
	if err != nil {
		t.Fatal(err)
	}
	parentID := initial.output.ID
	chain := []exploreProcessResult{initial}
	allIDs := map[string]bool{parentID: true}
	for depth := 1; depth <= explore.MaxFollowUps; depth++ {
		result := runSuccessfulExploreProcess(t, executable, repoDir, "--follow-up", parentID, "continue", "depth", fmt.Sprint(depth))
		if strings.Contains(result.stderr, "explore: searching") {
			t.Fatalf("depth %d unexpectedly repeated semantic retrieval:\n%s", depth, result.stderr)
		}
		if allIDs[result.output.ID] {
			t.Fatalf("depth %d reused search ID %s", depth, result.output.ID)
		}
		allIDs[result.output.ID] = true
		chain = append(chain, result)
		session, readErr := store.Read(result.output.ID)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if session.Depth != depth || session.ParentID != parentID {
			t.Fatalf("depth %d session = %#v", depth, session)
		}
		parentID = result.output.ID
	}
	if got := embeddingStats.calls(); got != initialEmbeddingCalls {
		t.Fatalf("context-preserving follow-ups made embedding calls: got %d, want %d", got, initialEmbeddingCalls)
	}

	reset := runSuccessfulExploreProcess(t, executable, repoDir, "--follow-up", parentID, "reset", "with", "fresh", "context")
	if !strings.Contains(reset.stderr, "explore: searching") {
		t.Fatalf("fourth follow-up did not report fresh semantic retrieval:\n%s", reset.stderr)
	}
	if allIDs[reset.output.ID] {
		t.Fatalf("fourth follow-up reused search ID %s", reset.output.ID)
	}
	resetSession, err := store.Read(reset.output.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resetSession.Depth != 0 || resetSession.ParentID != "" {
		t.Fatalf("reset session = %#v", resetSession)
	}
	if got := embeddingStats.calls(); got <= initialEmbeddingCalls {
		t.Fatalf("fourth follow-up embedding calls = %d, want > %d", got, initialEmbeddingCalls)
	}
	if calls, outputs := responseStats.readCounts(); calls != 1 || outputs != 1 {
		t.Fatalf("read tool calls/observed outputs = %d/%d, want 1/1", calls, outputs)
	}
	chain = append(chain, reset)
	assertExploreChainLog(t, projectID, chain)
}

func TestExploreCommandEndToEndBatchesAndForks(t *testing.T) {
	repoDir := newExploreE2ERepository(t)
	executable := buildExploreE2EBinary(t)
	responses, responseStats := newExploreE2EResponsesServer(t)
	defer responses.Close()
	embeddings, _ := newSearchEmbeddingsServer(t)
	defer embeddings.Close()
	configureExploreE2EEnvironment(t, responses.URL, embeddings.URL)
	projectID := runProjectIDProcess(t, executable, repoDir)

	initial := runConcurrentExploreProcesses(t, executable, repoDir, [][]string{
		{"find", "alpha"},
		{"find", "beta"},
		{"find", "gamma"},
	})
	assertDistinctExploreOutputs(t, initial)
	if got := responseStats.answerBatchSizes(); !slices.Equal(got, []int{3}) {
		t.Fatalf("initial provider batch sizes = %v, want [3]", got)
	}

	parentID := initial[0].output.ID
	siblings := runConcurrentExploreProcesses(t, executable, repoDir, [][]string{
		{"--follow-up", parentID, "sibling", "one"},
		{"--follow-up", parentID, "sibling", "two"},
		{"--follow-up", parentID, "sibling", "three"},
	})
	assertDistinctExploreOutputs(t, siblings)
	if got := responseStats.answerBatchSizes(); !slices.Equal(got, []int{3, 3}) {
		t.Fatalf("same-parent provider batch sizes = %v, want [3 3]", got)
	}
	store, err := explore.NewStore(projectMetadataDir(t, repoDir))
	if err != nil {
		t.Fatal(err)
	}
	for _, sibling := range siblings {
		session, readErr := store.Read(sibling.output.ID)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if session.ParentID != parentID || session.Depth != 1 {
			t.Fatalf("sibling session = %#v", session)
		}
	}
	nestedParentID := siblings[0].output.ID
	nested := runConcurrentExploreProcesses(t, executable, repoDir, [][]string{
		{"--follow-up", nestedParentID, "nested", "one"},
		{"--follow-up", nestedParentID, "nested", "two"},
		{"--follow-up", nestedParentID, "nested", "three"},
	})
	assertDistinctExploreOutputs(t, nested)
	if got := responseStats.answerBatchSizes(); !slices.Equal(got, []int{3, 3, 3}) {
		t.Fatalf("nested same-parent provider batch sizes = %v, want [3 3 3]", got)
	}
	for _, child := range nested {
		session, readErr := store.Read(child.output.ID)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if session.ParentID != nestedParentID || session.Depth != 2 {
			t.Fatalf("nested session = %#v", session)
		}
	}

	differentParents := runConcurrentExploreProcesses(t, executable, repoDir, [][]string{
		{"--follow-up", siblings[0].output.ID, "branch", "zero"},
		{"--follow-up", siblings[1].output.ID, "branch", "one"},
	})
	assertDistinctExploreOutputs(t, differentParents)
	if got := responseStats.answerBatchSizes(); !slices.Equal(got, []int{3, 3, 3, 1, 1}) {
		t.Fatalf("different-parent provider batch sizes = %v, want [3 3 3 1 1]", got)
	}
	assertExploreBatchLog(t, projectID)
}

func TestExploreCommandEndToEndRejectsUnknownIDWithoutProvider(t *testing.T) {
	repoDir := newExploreE2ERepository(t)
	executable := buildExploreE2EBinary(t)
	responses, responseStats := newExploreE2EResponsesServer(t)
	defer responses.Close()
	embeddings, embeddingStats := newSearchEmbeddingsServer(t)
	defer embeddings.Close()
	configureExploreE2EEnvironment(t, responses.URL, embeddings.URL)

	result := runExploreProcess(t.Context(), executable, repoDir,
		"--follow-up", "AAAAAAAAAAAAAAAAAAAAAAAAAA", "continue")
	if result.err == nil {
		t.Fatal("unknown follow-up ID succeeded")
	}
	if result.stdout != "" {
		t.Fatalf("unknown ID stdout = %q, want empty", result.stdout)
	}
	if !strings.Contains(result.stderr, "unknown explore search ID") {
		t.Fatalf("unknown ID stderr = %q", result.stderr)
	}
	if got := responseStats.requestCount(); got != 0 {
		t.Fatalf("unknown ID provider requests = %d, want 0", got)
	}
	if got := embeddingStats.calls(); got != 0 {
		t.Fatalf("unknown ID embedding requests = %d, want 0", got)
	}
}

type exploreE2EResponseStats struct {
	mu              sync.Mutex
	requests        int
	answerBatchIDs  [][]string
	readToolCalls   int
	readToolOutputs int
	readIssued      map[string]bool
}

func newExploreE2EResponsesServer(t *testing.T) (*httptest.Server, *exploreE2EResponseStats) {
	t.Helper()
	stats := &exploreE2EResponseStats{readIssued: make(map[string]bool)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("responses path = %s", r.URL.Path)
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read response request: %v", err)
			http.Error(w, "read request", http.StatusBadRequest)
			return
		}
		itemIDs, userText, err := currentExploreRequest(body)
		if err != nil {
			t.Errorf("parse explore request: %v\n%s", err, body)
			http.Error(w, "parse request", http.StatusBadRequest)
			return
		}

		bodyText := string(body)
		stats.mu.Lock()
		stats.requests++
		readKey := strings.Join(itemIDs, ",")
		needsRead := strings.Contains(userText, "read-owner") && !stats.readIssued[readKey]
		if needsRead {
			stats.readIssued[readKey] = true
			stats.readToolCalls++
		} else {
			stats.answerBatchIDs = append(stats.answerBatchIDs, slices.Clone(itemIDs))
			if strings.Contains(userText, "read-owner") && strings.Contains(bodyText, "func Answer") {
				stats.readToolOutputs++
			}
		}
		stats.mu.Unlock()

		var response string
		if needsRead {
			response = responseWithToolCalls("resp_explore_read", toolCallSpec{
				ID: "fc_explore_read", CallID: "call_explore_read", Name: "read_file",
				Arguments: `{"path":"main.go","start_line":1,"end_line":3}`,
			})
		} else {
			answers := make([]explore.Answer, 0, len(itemIDs))
			for _, itemID := range itemIDs {
				answer := "e2e context for " + itemID
				if strings.Contains(userText, "read-owner") {
					answer = "main.go:3 owns Answer"
				}
				answers = append(answers, explore.Answer{ItemID: itemID, Answer: answer})
			}
			answerJSON, marshalErr := json.Marshal(map[string]any{"answers": answers})
			if marshalErr != nil {
				t.Errorf("marshal explore answer: %v", marshalErr)
				http.Error(w, "marshal answer", http.StatusInternalServerError)
				return
			}
			response = responseWithText("resp_explore_answer", string(answerJSON))
		}
		if strings.Contains(bodyText, `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: %s\n\n", streamCompletedEvent(response))
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, response)
	}))
	return server, stats
}

func currentExploreRequest(body []byte) ([]string, string, error) {
	var payload struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, "", err
	}
	for index := len(payload.Input) - 1; index >= 0; index-- {
		var message struct {
			Role    string `json:"role"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(payload.Input[index], &message); err != nil {
			return nil, "", err
		}
		if message.Role != "user" {
			continue
		}
		var messageText strings.Builder
		for _, content := range message.Content {
			messageText.WriteString(content.Text)
		}
		const promptPrefix = "Exploration input JSON:\n"
		if !strings.HasPrefix(messageText.String(), promptPrefix) {
			continue
		}
		var envelope struct {
			Items []struct {
				ItemID   string `json:"item_id"`
				Question string `json:"question"`
			} `json:"items"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(messageText.String(), promptPrefix)), &envelope); err != nil {
			return nil, "", err
		}
		ids := make([]string, 0, len(envelope.Items))
		questions := make([]string, 0, len(envelope.Items))
		for _, item := range envelope.Items {
			ids = append(ids, item.ItemID)
			questions = append(questions, item.Question)
		}
		if len(ids) == 0 {
			return nil, "", fmt.Errorf("last user message has no explore item IDs")
		}
		return ids, strings.Join(questions, "\n"), nil
	}
	return nil, "", fmt.Errorf("request has no user message")
}

func (s *exploreE2EResponseStats) answerBatchSizes() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	sizes := make([]int, len(s.answerBatchIDs))
	for index, ids := range s.answerBatchIDs {
		sizes[index] = len(ids)
	}
	return sizes
}

func (s *exploreE2EResponseStats) readCounts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readToolCalls, s.readToolOutputs
}

func (s *exploreE2EResponseStats) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

type exploreProcessResult struct {
	output explore.Output
	stdout string
	stderr string
	err    error
}

func runSuccessfulExploreProcess(t *testing.T, executable, repoDir string, args ...string) exploreProcessResult {
	t.Helper()
	result := runExploreProcess(t.Context(), executable, repoDir, args...)
	assertSuccessfulExploreProcess(t, &result)
	return result
}

func runConcurrentExploreProcesses(t *testing.T, executable, repoDir string, args [][]string) []exploreProcessResult {
	t.Helper()
	ctx := t.Context()
	start := make(chan struct{})
	results := make([]exploreProcessResult, len(args))
	var wait sync.WaitGroup
	for index := range args {
		wait.Go(func() {
			<-start
			results[index] = runExploreProcess(ctx, executable, repoDir, args[index]...)
		})
	}
	close(start)
	wait.Wait()
	for index := range results {
		assertSuccessfulExploreProcess(t, &results[index])
	}
	return results
}

func runExploreProcess(ctx context.Context, executable, repoDir string, args ...string) exploreProcessResult {
	return runGitAgentProcess(ctx, executable, repoDir, append([]string{"explore"}, args...)...)
}

func runGitAgentProcess(ctx context.Context, executable, repoDir string, args ...string) exploreProcessResult {
	command := exec.CommandContext(ctx, executable, args...)
	command.Dir = repoDir
	command.Env = os.Environ()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return exploreProcessResult{stdout: stdout.String(), stderr: stderr.String(), err: err}
}

func runProjectIDProcess(t *testing.T, executable, repoDir string) string {
	t.Helper()
	result := runGitAgentProcess(t.Context(), executable, repoDir, "project_id")
	if result.err != nil {
		t.Fatalf("project_id failed: %v\nstderr:\n%s", result.err, result.stderr)
	}
	if result.stderr != "" {
		t.Fatalf("project_id stderr = %q", result.stderr)
	}
	projectID := strings.TrimSuffix(result.stdout, "\n")
	if len(projectID) != 64 || result.stdout != projectID+"\n" {
		t.Fatalf("project_id stdout = %q", result.stdout)
	}
	return projectID
}

func assertExploreChainLog(t *testing.T, projectID string, chain []exploreProcessResult) {
	t.Helper()
	lines := readExploreLogLines(t, projectID)
	if len(lines) != len(chain) {
		t.Fatalf("explore log lines = %d, want %d\n%s", len(lines), len(chain), strings.Join(lines, "\n"))
	}
	parentID := ""
	for index, result := range chain {
		branch := index > 0 && index <= explore.MaxFollowUps
		depth := index
		if !branch {
			depth = 0
			parentID = ""
		}
		for _, want := range []string{
			" mode=unbatched ", fmt.Sprintf(" branch=%t ", branch), " project_id=" + projectID + " ",
			" size=1 ", " item=" + result.output.ID + " ", " parent=" + parentID + " ",
			fmt.Sprintf(" depth=%d ", depth), " query=[redacted]",
		} {
			if !strings.Contains(lines[index], want) {
				t.Errorf("chain log line %d missing %q: %s", index, want, lines[index])
			}
		}
		if strings.Contains(lines[index], "read-owner") || strings.Contains(lines[index], "continue depth") || strings.Contains(lines[index], "reset with fresh context") {
			t.Errorf("query leaked in chain log: %s", lines[index])
		}
		parentID = result.output.ID
	}
}

func assertExploreBatchLog(t *testing.T, projectID string) {
	t.Helper()
	lines := readExploreLogLines(t, projectID)
	if len(lines) != 11 {
		t.Fatalf("explore batch log lines = %d, want 11\n%s", len(lines), strings.Join(lines, "\n"))
	}
	logText := strings.Join(lines, "\n")
	for token, want := range map[string]int{
		" mode=batched ":   9,
		" mode=unbatched ": 2,
		" branch=false ":   3,
		" branch=true ":    8,
		" size=3 ":         9,
		" size=1 ":         2,
	} {
		if got := strings.Count(logText, token); got != want {
			t.Errorf("log token %q count = %d, want %d\n%s", token, got, want, logText)
		}
	}
	for _, line := range lines {
		if !strings.Contains(line, " project_id="+projectID+" ") || !strings.Contains(line, " batch=batch-") || !strings.HasSuffix(line, " query=[redacted]") {
			t.Errorf("incomplete batch log line: %s", line)
		}
		for _, query := range []string{
			"find alpha", "find beta", "find gamma",
			"sibling one", "sibling two", "sibling three",
			"nested one", "nested two", "nested three",
			"branch zero", "branch one",
		} {
			if strings.Contains(line, query) {
				t.Errorf("query %q leaked in batch log: %s", query, line)
			}
		}
	}
}

func readExploreLogLines(t *testing.T, projectID string) []string {
	t.Helper()
	path := filepath.Join(os.Getenv("XDG_STATE_HOME"), "git-agent", projectID, "explore.log")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func assertSuccessfulExploreProcess(t *testing.T, result *exploreProcessResult) {
	t.Helper()
	if result.err != nil {
		t.Fatalf("explore process failed: %v\nstdout:\n%s\nstderr:\n%s", result.err, result.stdout, result.stderr)
	}
	if !strings.HasSuffix(result.stdout, "\n") || strings.Count(result.stdout, "\n") != 1 {
		t.Fatalf("stdout is not exactly one newline-terminated object: %q", result.stdout)
	}
	decoder := json.NewDecoder(strings.NewReader(result.stdout))
	if err := decoder.Decode(&result.output); err != nil {
		t.Fatalf("decode explore stdout: %v\n%s", err, result.stdout)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		t.Fatalf("stdout contains more than one JSON value: %v\n%s", err, result.stdout)
	}
	if result.output.ID == "" || result.output.Answer == "" {
		t.Fatalf("explore output = %#v", result.output)
	}
	if strings.Contains(result.stdout, "explore:") {
		t.Fatalf("progress leaked to stdout: %q", result.stdout)
	}
}

func assertDistinctExploreOutputs(t *testing.T, results []exploreProcessResult) {
	t.Helper()
	ids := make(map[string]bool, len(results))
	for _, result := range results {
		if ids[result.output.ID] {
			t.Fatalf("concurrent explores reused ID %s", result.output.ID)
		}
		ids[result.output.ID] = true
	}
}

func buildExploreE2EBinary(t *testing.T) string {
	t.Helper()
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(t.TempDir(), "git-agent")
	command := exec.CommandContext(t.Context(), "go", "build", "-o", executable, "./cmd/git-agent")
	command.Dir = repoRoot
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("build git-agent: %v\n%s", err, output)
	}
	return executable
}

func newExploreE2ERepository(t *testing.T) string {
	t.Helper()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	writeFixtureFile(t, filepath.Join(repoDir, "main.go"), "package demo\n\nfunc Answer() int { return 42 }\n")
	return repoDir
}

func configureExploreE2EEnvironment(t *testing.T, responsesURL, embeddingsURL string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", responsesURL)
	t.Setenv("OPENAI_MODEL", "test-model")
	t.Setenv("OPENAI_EMBEDDING_BASE_URL", embeddingsURL)
	t.Setenv("OPENAI_EMBEDDING_DIMENSIONS", "1024")
}

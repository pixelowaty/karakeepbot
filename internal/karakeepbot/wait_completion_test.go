package karakeepbot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	karakeep "github.com/Madh93/go-karakeep"
	tgbotapi "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/Madh93/karakeepbot/internal/config"
	"github.com/Madh93/karakeepbot/internal/fileprocessor"
	"github.com/Madh93/karakeepbot/internal/filevalidator"
	"github.com/Madh93/karakeepbot/internal/logging"
)

// tagServer is a fake Karakeep API server for testing tag polling and full
// handler flows (assets, bookmarks, tags).
type tagServer struct {
	server *httptest.Server
	client *karakeep.ClientWithResponses

	mu         sync.Mutex
	bookmarks  map[string]*karakeep.Bookmark
	getCalls   int
	getErrors  int
	nextBoomID int
}

func newTagServer(t *testing.T) *tagServer {
	t.Helper()

	s := &tagServer{
		bookmarks: make(map[string]*karakeep.Bookmark),
	}
	s.server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.server.Close)

	client, err := karakeep.NewClientWithResponses(s.server.URL + "/api/v1")
	if err != nil {
		t.Fatalf("failed to create karakeep client: %v", err)
	}
	s.client = client

	return s
}

func (s *tagServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && r.URL.Path == "/api/v1/assets" {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "missing file part: "+err.Error(), http.StatusBadRequest)
			return
		}
		body, readErr := io.ReadAll(file)
		_ = file.Close()
		if readErr != nil {
			http.Error(w, readErr.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"assetId":"asset-123","fileName":%q,"contentType":%q,"size":%d}`, header.Filename, header.Header.Get("Content-Type"), len(body))
		return
	}

	if r.Method == http.MethodPost && r.URL.Path == "/api/v1/bookmarks" {
		s.mu.Lock()
		s.nextBoomID++
		id := fmt.Sprintf("bm-created-%d", s.nextBoomID)
		bm := &karakeep.Bookmark{
			Id:            id,
			TaggingStatus: ptrStatus("pending"),
		}
		s.bookmarks[id] = bm
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(bm)
		return
	}

	const prefix = "/api/v1/bookmarks/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	if rest == "" {
		http.NotFound(w, r)
		return
	}

	// POST /api/v1/bookmarks/{id}/tags
	if r.Method == http.MethodPost && strings.HasSuffix(rest, "/tags") {
		id := strings.TrimSuffix(rest, "/tags")
		var payload struct {
			Tags []struct {
				TagName string `json:"tagName"`
			} `json:"tags"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		if bm, exists := s.bookmarks[id]; exists {
			for _, t := range payload.Tags {
				bm.Tags = append(bm.Tags, struct {
					AttachedBy karakeep.BookmarkTagsAttachedBy `json:"attachedBy"`
					Id         string                          `json:"id"`
					Name       string                          `json:"name"`
				}{
					AttachedBy: karakeep.Human,
					Name:       t.TagName,
				})
			}
		}
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"attached":[]}`))
		return
	}

	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}

	id := rest
	s.mu.Lock()
	s.getCalls++
	if s.getErrors > 0 {
		s.getErrors--
		s.mu.Unlock()
		http.Error(w, "temporary error", http.StatusInternalServerError)
		return
	}
	bm, exists := s.bookmarks[id]
	var snapshot *karakeep.Bookmark
	if exists {
		cp := *bm
		cp.Tags = append([]struct {
			AttachedBy karakeep.BookmarkTagsAttachedBy `json:"attachedBy"`
			Id         string                          `json:"id"`
			Name       string                          `json:"name"`
		}(nil), bm.Tags...)
		snapshot = &cp
	}
	s.mu.Unlock()

	if snapshot == nil {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(snapshot)
}

// setBookmark sets the bookmark state for the given ID.
func (s *tagServer) setBookmark(id string, status string, tags []karakeep.BookmarkTagsAttachedBy) {
	s.mu.Lock()
	defer s.mu.Unlock()

	b := &karakeep.Bookmark{
		Id:            id,
		TaggingStatus: ptrStatus(status),
	}
	if status == "success" {
		for _, name := range tags {
			b.Tags = append(b.Tags, struct {
				AttachedBy karakeep.BookmarkTagsAttachedBy `json:"attachedBy"`
				Id         string                          `json:"id"`
				Name       string                          `json:"name"`
			}{
				AttachedBy: name,
				Name:       string(name),
			})
		}
	}
	s.bookmarks[id] = b
}

// appendAITags updates an existing bookmark to success and appends AI tags
// while preserving existing tags (e.g., #telegram).
func (s *tagServer) appendAITags(id string, aiTags ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	bm, exists := s.bookmarks[id]
	if !exists {
		return
	}
	bm.TaggingStatus = ptrStatus("success")
	for _, name := range aiTags {
		bm.Tags = append(bm.Tags, struct {
			AttachedBy karakeep.BookmarkTagsAttachedBy `json:"attachedBy"`
			Id         string                          `json:"id"`
			Name       string                          `json:"name"`
		}{
			AttachedBy: karakeep.Ai,
			Name:       name,
		})
	}
}

func (s *tagServer) setTransientGetErrors(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getErrors = n
}

func (s *tagServer) totalGetCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getCalls
}

// ptrStatus returns a pointer to a BookmarkTaggingStatus.
func ptrStatus(s string) *karakeep.BookmarkTaggingStatus {
	status := karakeep.BookmarkTaggingStatus(s)
	return &status
}

// waitForBookmark creates a test KarakeepBot connected to the tag server.
func waitForBookmark(t *testing.T, kk *tagServer) *KarakeepBot {
	t.Helper()

	botAPI, err := tgbotapi.New("test-token", tgbotapi.WithServerURL("http://localhost"), tgbotapi.WithSkipGetMe())
	if err != nil {
		t.Fatalf("failed to create telegram bot: %v", err)
	}

	return &KarakeepBot{
		karakeep:       &Karakeep{ClientWithResponses: kk.client},
		telegram:       &Telegram{Bot: botAPI},
		logger:         logging.New(&config.LoggingConfig{Level: "error", Format: "text", Output: "stderr"}),
		waitInterval:   0,
		bgPollInterval: 20 * time.Millisecond,
		bgPollTimeout:  2 * time.Second,
	}
}

func TestKarakeepBookmark_AttrsNilTaggingStatus(t *testing.T) {
	t.Parallel()

	bm := KarakeepBookmark{Id: "bm-nil"}
	attrs := bm.Attrs()
	found := false
	for i := 0; i+1 < len(attrs); i += 2 {
		if attrs[i] == "tagging_status" && attrs[i+1] == "<nil>" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected tagging_status=<nil> in attrs, got %v", attrs)
	}
}

func TestNewTelegramHTTPClient(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	logger := logging.New(&config.LoggingConfig{Level: "error", Format: "text", Output: "stderr"})

	// When ProxyEnabled is false, Transport must be untyped nil (no panic).
	client := newTelegramHTTPClient(logger, &config.TelegramConfig{ProxyEnabled: false})
	if client.Transport != nil {
		t.Fatalf("expected nil Transport when proxy is disabled, got %T", client.Transport)
	}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("unexpected error using non-proxy HTTP client: %v", err)
	}
	_ = resp.Body.Close()

	// When ProxyEnabled is true, Transport must configure the proxy.
	proxyClient := newTelegramHTTPClient(logger, &config.TelegramConfig{
		ProxyEnabled: true,
		ProxyURL:     "http://127.0.0.1:8080",
	})
	tr, ok := proxyClient.Transport.(*http.Transport)
	if !ok || tr == nil || tr.Proxy == nil {
		t.Fatalf("expected configured *http.Transport with Proxy when ProxyEnabled=true, got %#v", proxyClient.Transport)
	}
}

func TestWaitForTagCompletion_ImmediateSuccess(t *testing.T) {
	t.Parallel()

	server := newTagServer(t)
	kb := waitForBookmark(t, server)

	server.setBookmark("bm-1", "success", []karakeep.BookmarkTagsAttachedBy{"ai", "karakeep"})

	bookmark := &KarakeepBookmark{
		Id:            "bm-1",
		TaggingStatus: ptrStatus("success"),
	}

	result, err := kb.waitForTagCompletion(context.Background(), bookmark)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected result, got nil")
	}
	if result.TagsUpdatedAfterTimeout {
		t.Error("expected TagsUpdatedAfterTimeout=false for immediate success")
	}
}

func TestWaitForTagCompletion_DelayedSuccess(t *testing.T) {
	t.Parallel()

	server := newTagServer(t)
	kb := waitForBookmark(t, server)
	kb.waitInterval = 1

	server.setBookmark("bm-2", "pending", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	type resultPair struct {
		result *TagCompletionResult
		err    error
	}

	done := make(chan resultPair, 1)
	go func() {
		result, err := kb.waitForTagCompletion(ctx, &KarakeepBookmark{
			Id:            "bm-2",
			TaggingStatus: ptrStatus("pending"),
		})
		done <- resultPair{result, err}
	}()

	time.Sleep(500 * time.Millisecond)
	server.setBookmark("bm-2", "success", []karakeep.BookmarkTagsAttachedBy{"ai", "karakeep"})

	pair := <-done
	if pair.err != nil {
		t.Fatalf("unexpected error: %v", pair.err)
	}
	if pair.result == nil {
		t.Fatal("expected result, got nil")
	}
	if pair.result.TagsUpdatedAfterTimeout {
		t.Error("expected TagsUpdatedAfterTimeout=false when tags complete during wait loop")
	}
}

func TestWaitForTagCompletion_Timeout(t *testing.T) {
	t.Parallel()

	server := newTagServer(t)
	kb := waitForBookmark(t, server)

	server.setBookmark("bm-3", "pending", nil)

	result, err := kb.waitForTagCompletion(context.Background(), &KarakeepBookmark{
		Id:            "bm-3",
		TaggingStatus: ptrStatus("pending"),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected result, got nil")
	}
	if !result.TagsUpdatedAfterTimeout {
		t.Error("expected TagsUpdatedAfterTimeout=true on timeout so background polling starts")
	}
}

func TestWaitForTagCompletion_Failure(t *testing.T) {
	t.Parallel()

	server := newTagServer(t)
	kb := waitForBookmark(t, server)

	server.setBookmark("bm-4", "failure", nil)

	_, err := kb.waitForTagCompletion(context.Background(), &KarakeepBookmark{
		Id:            "bm-4",
		TaggingStatus: ptrStatus("failure"),
	})
	if err == nil {
		t.Fatal("expected error for failed tagging")
	}
}

func TestWaitForTagCompletion_ContextCancelled(t *testing.T) {
	t.Parallel()

	server := newTagServer(t)
	kb := waitForBookmark(t, server)
	kb.waitInterval = 10

	server.setBookmark("bm-ctx", "pending", nil)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	_, err := kb.waitForTagCompletion(ctx, &KarakeepBookmark{
		Id:            "bm-ctx",
		TaggingStatus: ptrStatus("pending"),
	})
	if err == nil {
		t.Fatal("expected context cancellation error")
	}
}

func TestContinueWaitForTags_DelayedTags(t *testing.T) {
	t.Parallel()

	server := newTagServer(t)
	kb := waitForBookmark(t, server)

	server.setBookmark("bm-5", "pending", nil)

	var onUpdateCalled atomic.Bool
	var onUpdateTags atomic.Int32

	done := kb.ContinueWaitForTags(context.Background(), &KarakeepBookmark{
		Id:            "bm-5",
		TaggingStatus: ptrStatus("pending"),
	}, func(updated *KarakeepBookmark) {
		onUpdateCalled.Store(true)
		onUpdateTags.Store(int32(len(updated.Tags)))
	})

	time.Sleep(50 * time.Millisecond)
	server.setBookmark("bm-5", "success", []karakeep.BookmarkTagsAttachedBy{"ai", "karakeep"})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ContinueWaitForTags to finish")
	}

	if !onUpdateCalled.Load() {
		t.Error("expected onUpdate callback to be called")
	}
	if onUpdateTags.Load() == 0 {
		t.Error("expected onUpdateTags > 0")
	}
}

func TestContinueWaitForTags_SuccessWithZeroTagsStopsImmediately(t *testing.T) {
	t.Parallel()

	server := newTagServer(t)
	kb := waitForBookmark(t, server)
	kb.bgPollTimeout = 5 * time.Second

	server.setBookmark("bm-empty", "success", nil)

	var onUpdateCalled atomic.Bool
	done := kb.ContinueWaitForTags(context.Background(), &KarakeepBookmark{
		Id:            "bm-empty",
		TaggingStatus: ptrStatus("pending"),
	}, func(updated *KarakeepBookmark) {
		onUpdateCalled.Store(true)
	})

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected ContinueWaitForTags to exit promptly when tagging succeeds with 0 tags")
	}

	if onUpdateCalled.Load() {
		t.Error("expected onUpdate NOT to be called when 0 tags were generated")
	}
	if calls := server.totalGetCalls(); calls != 1 {
		t.Errorf("expected exactly 1 poll call, got %d", calls)
	}
}

func TestContinueWaitForTags_TransientErrorRetries(t *testing.T) {
	t.Parallel()

	server := newTagServer(t)
	kb := waitForBookmark(t, server)

	server.setBookmark("bm-retry", "success", []karakeep.BookmarkTagsAttachedBy{"ai"})
	server.setTransientGetErrors(2)

	var onUpdateCalled atomic.Bool
	done := kb.ContinueWaitForTags(context.Background(), &KarakeepBookmark{
		Id:            "bm-retry",
		TaggingStatus: ptrStatus("pending"),
	}, func(updated *KarakeepBookmark) {
		onUpdateCalled.Store(true)
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ContinueWaitForTags after transient errors")
	}

	if !onUpdateCalled.Load() {
		t.Error("expected onUpdate to be called after transient errors recovered")
	}
}

func TestContinueWaitForTags_NoTagsFoundTimeout(t *testing.T) {
	t.Parallel()

	server := newTagServer(t)
	kb := waitForBookmark(t, server)
	kb.bgPollInterval = 15 * time.Millisecond
	kb.bgPollTimeout = 80 * time.Millisecond

	server.setBookmark("bm-6", "pending", nil)

	var onUpdateCalled atomic.Bool
	done := kb.ContinueWaitForTags(context.Background(), &KarakeepBookmark{
		Id:            "bm-6",
		TaggingStatus: ptrStatus("pending"),
	}, func(updated *KarakeepBookmark) {
		onUpdateCalled.Store(true)
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for bgPollTimeout")
	}

	if onUpdateCalled.Load() {
		t.Error("expected onUpdate NOT to be called when tags stay pending until timeout")
	}
}

func TestContinueWaitForTags_AlreadyComplete(t *testing.T) {
	t.Parallel()

	server := newTagServer(t)
	kb := waitForBookmark(t, server)

	server.setBookmark("bm-7", "success", []karakeep.BookmarkTagsAttachedBy{"ai"})

	var onUpdateCalled atomic.Bool
	done := kb.ContinueWaitForTags(context.Background(), &KarakeepBookmark{
		Id:            "bm-7",
		TaggingStatus: ptrStatus("success"),
	}, func(updated *KarakeepBookmark) {
		onUpdateCalled.Store(true)
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for already-ready tags callback")
	}

	if !onUpdateCalled.Load() {
		t.Error("expected onUpdate callback to be called when tags are already ready")
	}
}

func TestContinueWaitForTags_TaggingFailure(t *testing.T) {
	t.Parallel()

	server := newTagServer(t)
	kb := waitForBookmark(t, server)

	server.setBookmark("bm-8", "failure", nil)

	var onUpdateCalled atomic.Bool
	done := kb.ContinueWaitForTags(context.Background(), &KarakeepBookmark{
		Id:            "bm-8",
		TaggingStatus: ptrStatus("failure"),
	}, func(updated *KarakeepBookmark) {
		onUpdateCalled.Store(true)
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ContinueWaitForTags to exit on failure")
	}

	if onUpdateCalled.Load() {
		t.Error("expected onUpdate callback NOT to be called for failed tagging")
	}
}

func TestTagCompletionResult_HashtagsOnSuccess(t *testing.T) {
	t.Parallel()

	server := newTagServer(t)
	kb := waitForBookmark(t, server)

	server.setBookmark("bm-9", "success", []karakeep.BookmarkTagsAttachedBy{"golang", "pdf"})

	result, err := kb.waitForTagCompletion(context.Background(), &KarakeepBookmark{
		Id:            "bm-9",
		TaggingStatus: ptrStatus("success"),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	hashtags := result.Bookmark.Hashtags()
	if hashtags != "#golang #pdf" {
		t.Errorf("expected '#golang #pdf', got %q", hashtags)
	}
}

func TestTagCompletionResult_HashtagsOnTimeout(t *testing.T) {
	t.Parallel()

	server := newTagServer(t)
	kb := waitForBookmark(t, server)

	server.setBookmark("bm-10", "pending", nil)

	result, err := kb.waitForTagCompletion(context.Background(), &KarakeepBookmark{
		Id:            "bm-10",
		TaggingStatus: ptrStatus("pending"),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	hashtags := result.Bookmark.Hashtags()
	if hashtags != "" {
		t.Errorf("expected empty hashtags for pending/timeout, got %q", hashtags)
	}
}

func TestWaitForTagCompletion_NonExistentBookmark(t *testing.T) {
	t.Parallel()

	server := newTagServer(t)
	kb := waitForBookmark(t, server)

	_, err := kb.waitForTagCompletion(context.Background(), &KarakeepBookmark{
		Id:            "nonexistent",
		TaggingStatus: ptrStatus("pending"),
	})
	if err == nil {
		t.Fatal("expected error for nonexistent bookmark")
	}
}

func TestContinueWaitForTags_ContextCancellation(t *testing.T) {
	t.Parallel()

	server := newTagServer(t)
	kb := waitForBookmark(t, server)
	kb.bgPollInterval = 1 * time.Second
	kb.bgPollTimeout = 10 * time.Second

	server.setBookmark("bm-cancel", "pending", nil)

	ctx, cancel := context.WithCancel(context.Background())

	var onUpdateCalled atomic.Bool
	done := kb.ContinueWaitForTags(ctx, &KarakeepBookmark{
		Id:            "bm-cancel",
		TaggingStatus: ptrStatus("pending"),
	}, func(updated *KarakeepBookmark) {
		onUpdateCalled.Store(true)
	})

	cancel()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for background goroutine to exit after context cancellation")
	}

	if onUpdateCalled.Load() {
		t.Error("callback should not be called after context cancellation")
	}
}

func TestHandler_TextBookmark_BackgroundTags_NoDoubleHashtags(t *testing.T) {
	t.Parallel()

	tg := newTelegramMock(t)
	tagSrv := newTagServer(t)

	botAPI, err := tgbotapi.New(testToken, tgbotapi.WithServerURL(tg.server.URL), tgbotapi.WithSkipGetMe())
	if err != nil {
		t.Fatalf("failed to create telegram bot: %v", err)
	}

	kb := KarakeepBot{
		telegram:       &Telegram{Bot: botAPI},
		karakeep:       &Karakeep{ClientWithResponses: tagSrv.client},
		logger:         logging.New(&config.LoggingConfig{Level: "error", Format: "text", Output: "stderr"}),
		waitInterval:   0,
		bgPollInterval: 20 * time.Millisecond,
		bgPollTimeout:  2 * time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	kb.handler(ctx, nil, &TelegramUpdate{
		Message: &models.Message{
			ID:   42,
			Chat: models.Chat{ID: 12345, Type: models.ChatTypePrivate},
			From: &models.User{ID: 7, Username: "tester"},
			Text: "https://example.com",
		},
	})

	replies := tg.replies()
	if len(replies) != 1 || replies[0] != "https://example.com\n\n#telegram" {
		t.Fatalf("expected initial message %q, got %v", "https://example.com\n\n#telegram", replies)
	}

	// Simulate Karakeep finishing AI tagging in the background.
	tagSrv.appendAITags("bm-created-1", "ai", "golang")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(tg.edits()) > 0 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}

	edits := tg.edits()
	if len(edits) != 1 {
		t.Fatalf("expected 1 editMessageText call, got %d (%+v)", len(edits), edits)
	}
	if !edits[0].isText {
		t.Errorf("expected editMessageText, got %+v", edits[0])
	}
	wantText := "https://example.com\n\n#telegram #ai #golang"
	if edits[0].text != wantText {
		t.Errorf("expected edited text %q (without duplicate #telegram), got %q", wantText, edits[0].text)
	}
}

func TestHandler_PDFDocument_BackgroundTags_EmptyCaption(t *testing.T) {
	t.Parallel()

	tg := newTelegramMock(t)
	tg.fileBody = []byte(testPDF)
	tagSrv := newTagServer(t)

	botAPI, err := tgbotapi.New(testToken, tgbotapi.WithServerURL(tg.server.URL), tgbotapi.WithSkipGetMe())
	if err != nil {
		t.Fatalf("failed to create telegram bot: %v", err)
	}

	tempdir := t.TempDir()
	fp, err := fileprocessor.New(&config.FileProcessorConfig{
		Tempdir: tempdir,
		Maxsize: 1024,
		Timeout: 5,
	}, false, "")
	if err != nil {
		t.Fatalf("failed to create file processor: %v", err)
	}

	kb := KarakeepBot{
		telegram:       &Telegram{Bot: botAPI},
		karakeep:       &Karakeep{ClientWithResponses: tagSrv.client},
		fileProcessor:  fp,
		fileValidators: map[string]fileprocessor.Validator{"application/pdf": filevalidator.PDFValidator},
		maxFilesize:    1024,
		logger:         logging.New(&config.LoggingConfig{Level: "error", Format: "text", Output: "stderr"}),
		waitInterval:   0,
		bgPollInterval: 20 * time.Millisecond,
		bgPollTimeout:  2 * time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	kb.handler(ctx, nil, &TelegramUpdate{
		Message: &models.Message{
			ID:   43,
			Chat: models.Chat{ID: 12345, Type: models.ChatTypePrivate},
			From: &models.User{ID: 7, Username: "tester"},
			Document: &models.Document{
				FileID:   "doc-file-id",
				FileName: "paper.pdf",
				MimeType: "application/pdf",
				FileSize: int64(len(testPDF)),
			},
		},
	})

	sentDoc := tg.document()
	if sentDoc == nil {
		t.Fatal("expected sendDocument call")
	}
	if sentDoc.caption != "#telegram" {
		t.Errorf("expected initial caption '#telegram' without leading newlines, got %q", sentDoc.caption)
	}

	tagSrv.appendAITags("bm-created-1", "research", "pdf")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(tg.edits()) > 0 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}

	edits := tg.edits()
	if len(edits) != 1 {
		t.Fatalf("expected 1 editMessageCaption call, got %d (%+v)", len(edits), edits)
	}
	if edits[0].isText {
		t.Errorf("expected editMessageCaption, got %+v", edits[0])
	}
	if edits[0].caption != "#telegram #research #pdf" {
		t.Errorf("expected edited caption '#telegram #research #pdf', got %q", edits[0].caption)
	}
}

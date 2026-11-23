package karakeepbot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	karakeep "github.com/Madh93/go-karakeep"
	tgbotapi "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/Madh93/karakeepbot/internal/config"
	"github.com/Madh93/karakeepbot/internal/logging"
)

// tagServer is a minimal fake Karakeep API server that handles GET and PATCH
// /api/v1/bookmarks/:id for testing the tag completion wait and title update
// logic.
type tagServer struct {
	server *httptest.Server
	client *karakeep.ClientWithResponses

	mu          sync.Mutex
	bookmarks   map[string]*karakeep.Bookmark
	tagsReadyAt time.Time

	// patchCalls records the PATCH request bodies received, keyed by bookmark
	// ID, for asserting that summary is preserved alongside title.
	patchCalls map[string]map[string]json.RawMessage
}

func newTagServer(t *testing.T) *tagServer {
	t.Helper()

	s := &tagServer{
		bookmarks:  make(map[string]*karakeep.Bookmark),
		patchCalls: make(map[string]map[string]json.RawMessage),
	}
	s.server = httptest.NewServer(http.HandlerFunc(s.handle))

	client, err := karakeep.NewClientWithResponses(s.server.URL + "/api/v1")
	if err != nil {
		t.Fatalf("failed to create karakeep client: %v", err)
	}
	s.client = client

	return s
}

func (s *tagServer) handle(w http.ResponseWriter, r *http.Request) {
	// Expected path: /api/v1/bookmarks/{id}
	// The Karakeep client prepends "." to the operation path to make it relative,
	// preserving the /api/v1 prefix from the client's base URL.
	const prefix = "/api/v1/bookmarks/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, prefix)
	if id == "" {
		http.NotFound(w, r)
		return
	}

	if r.Method == http.MethodPatch {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid json"}`))
			return
		}
		s.mu.Lock()
		s.patchCalls[id] = body
		// Apply the title/summary patch to the stored bookmark.
		if bm, exists := s.bookmarks[id]; exists {
			if raw, ok := body["title"]; ok && string(raw) != "null" {
				var title string
				_ = json.Unmarshal(raw, &title)
				bm.Title = &title
			}
			if raw, ok := body["summary"]; ok {
				if string(raw) == "null" {
					bm.Summary = nil
				} else {
					var summary string
					_ = json.Unmarshal(raw, &summary)
					bm.Summary = &summary
				}
			}
		}
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
		return
	}

	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}

	s.mu.Lock()
	bm, exists := s.bookmarks[id]
	fail := !exists
	s.mu.Unlock()

	if fail {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(bm)
}

// setBookmarkFull sets a bookmark with full control over title, summary,
// summarization status, and tags.
func (s *tagServer) setBookmarkFull(id, status, summarizationStatus string, summary *string, title *string, tags []karakeep.BookmarkTagsAttachedBy) {
	s.mu.Lock()
	defer s.mu.Unlock()

	b := &karakeep.Bookmark{
		Id:                  id,
		TaggingStatus:       ptrStatus(status),
		SummarizationStatus: ptrSummarizationStatus(summarizationStatus),
		Summary:             summary,
		Title:               title,
	}
	for _, name := range tags {
		b.Tags = append(b.Tags, struct {
			AttachedBy karakeep.BookmarkTagsAttachedBy `json:"attachedBy"`
			Id         string                          `json:"id"`
			Name       string                          `json:"name"`
		}{
			AttachedBy: karakeep.BookmarkTagsAttachedBy(name),
			Name:       string(name),
		})
	}
	s.bookmarks[id] = b
}

// patchCall returns the raw JSON body of the last PATCH for the given ID.
func (s *tagServer) patchCall(id string) (map[string]json.RawMessage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.patchCalls[id]
	return c, ok
}

// ptrSummarizationStatus returns a pointer to a BookmarkSummarizationStatus.
func ptrSummarizationStatus(s string) *karakeep.BookmarkSummarizationStatus {
	status := karakeep.BookmarkSummarizationStatus(s)
	return &status
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
				AttachedBy: karakeep.BookmarkTagsAttachedBy(name),
				Name:       string(name),
			})
		}
	}
	s.bookmarks[id] = b
}

// ptrString returns a pointer to a string.
func ptrString(s string) *string {
	return &s
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
		karakeep:     &Karakeep{ClientWithResponses: kk.client},
		telegram:     &Telegram{Bot: botAPI},
		logger:       logging.New(&config.LoggingConfig{Level: "error", Format: "text", Output: "stderr"}),
		waitInterval: 1, // faster for tests
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
		Tags: []struct {
			AttachedBy karakeep.BookmarkTagsAttachedBy `json:"attachedBy"`
			Id         string                          `json:"id"`
			Name       string                          `json:"name"`
		}{{
			AttachedBy: "ai",
			Name:       "golang",
		}},
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

	// Start with pending
	server.setBookmark("bm-2", "pending", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Start polling in the background while another goroutine makes tags ready
	type resultPair struct {
		result *TagCompletionResult
		err    error
	}

	done := make(chan resultPair)

	go func() {
		result, err := kb.waitForTagCompletion(ctx, &KarakeepBookmark{
			Id:            "bm-2",
			TaggingStatus: ptrStatus("pending"),
		})
		done <- resultPair{result, err}
	}()

	// After 2 seconds, make tags ready
	time.Sleep(2 * time.Second)
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

	// Always pending
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

func TestContinueWaitForTags_DelayedTags(t *testing.T) {
	t.Parallel()

	server := newTagServer(t)
	kb := waitForBookmark(t, server)

	server.setBookmark("bm-5", "pending", nil)

	var onUpdateCalled bool
	var onUpdateTags int

	done := make(chan struct{})
	go func() {
		kb.ContinueWaitForTags(context.Background(), &KarakeepBookmark{
			Id:            "bm-5",
			TaggingStatus: ptrStatus("pending"),
		}, func(updated *KarakeepBookmark) {
			onUpdateCalled = true
			onUpdateTags = len(updated.Tags)
			close(done)
		})
	}()

	// After 3 seconds, make tags ready (giving the 15s initial delay + some buffer)
	time.Sleep(3 * time.Second)
	server.setBookmark("bm-5", "success", []karakeep.BookmarkTagsAttachedBy{"ai", "karakeep"})

	// Wait for the callback with a generous timeout
	select {
	case <-done:
		if !onUpdateCalled {
			t.Error("expected onUpdate callback to be called")
		}
		if onUpdateTags == 0 {
			t.Error("expected onUpdateTags > 0")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for tags to be discovered")
	}
}

func TestContinueWaitForTags_NoTagsFound(t *testing.T) {
	t.Parallel()

	server := newTagServer(t)
	kb := waitForBookmark(t, server)

	server.setBookmark("bm-6", "pending", nil)

	var onUpdateCalled bool
	done := make(chan struct{})
	go func() {
		kb.ContinueWaitForTags(context.Background(), &KarakeepBookmark{
			Id:            "bm-6",
			TaggingStatus: ptrStatus("pending"),
		}, func(updated *KarakeepBookmark) {
			onUpdateCalled = true
			close(done)
		})
	}()

	// Wait for the full 5-minute timeout but only wait a reasonable time for the test
	select {
	case <-done:
		if onUpdateCalled {
			t.Error("expected onUpdate callback NOT to be called for tags that never appear")
		}
	case <-time.After(45 * time.Second):
		// Expected: the callback is not called within 45s because tags are still pending
		// (the full 5-minute timeout has not yet elapsed)
	}
}

func TestContinueWaitForTags_AlreadyComplete(t *testing.T) {
	t.Parallel()

	server := newTagServer(t)
	kb := waitForBookmark(t, server)

	// Tags are already ready
	server.setBookmark("bm-7", "success", []karakeep.BookmarkTagsAttachedBy{"ai"})

	var onUpdateCalled bool
	done := make(chan struct{})
	go func() {
		kb.ContinueWaitForTags(context.Background(), &KarakeepBookmark{
			Id:            "bm-7",
			TaggingStatus: ptrStatus("success"),
		}, func(updated *KarakeepBookmark) {
			onUpdateCalled = true
			close(done)
		})
	}()

	select {
	case <-done:
		if !onUpdateCalled {
			t.Error("expected onUpdate callback to be called when tags are already ready")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for already-ready tags callback")
	}
}

func TestContinueWaitForTags_TaggingFailure(t *testing.T) {
	t.Parallel()

	server := newTagServer(t)
	kb := waitForBookmark(t, server)

	// Set bookmark to failure immediately
	server.setBookmark("bm-8", "failure", nil)

	var onUpdateCalled bool
	done := make(chan struct{})

	// Run ContinueWaitForTags in a goroutine with a timeout
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	go func() {
		kb.ContinueWaitForTags(ctx, &KarakeepBookmark{
			Id:            "bm-8",
			TaggingStatus: ptrStatus("failure"),
		}, func(updated *KarakeepBookmark) {
			onUpdateCalled = true
		})
		close(done)
	}()

	// Wait for the goroutine to finish (it should exit after detecting failure)
	select {
	case <-done:
		// The callback should NOT be called for failed tagging
		if onUpdateCalled {
			t.Error("expected onUpdate callback NOT to be called for failed tagging")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for ContinueWaitForTags to exit")
	}
}

func TestTagCompletionResult_HashtagsOnSuccess(t *testing.T) {
	t.Parallel()

	server := newTagServer(t)
	kb := waitForBookmark(t, server)

	// Bookmark with AI-generated tags
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

	// Bookmark that stays pending - will timeout
	// The server doesn't add tags for pending status, so the returned bookmark
	// will have no tags
	server.setBookmark("bm-10", "pending", nil)

	result, err := kb.waitForTagCompletion(context.Background(), &KarakeepBookmark{
		Id:            "bm-10",
		TaggingStatus: ptrStatus("pending"),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// When tagging is still pending and times out, the server's bookmark is returned
	// (which has no tags in the pending state)
	hashtags := result.Bookmark.Hashtags()
	if hashtags != "" {
		t.Errorf("expected empty hashtags for pending/timeout, got %q", hashtags)
	}
}

// TestWaitForTagCompletion_NonExistentBookmark verifies that a 404 on the
// first poll returns an error (not a panic or infinite loop).
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

// TestContinueWaitForTags_ContextCancellation verifies that the background
// goroutine exits cleanly when the context is cancelled.
func TestContinueWaitForTags_ContextCancellation(t *testing.T) {
	t.Parallel()

	server := newTagServer(t)
	kb := waitForBookmark(t, server)

	server.setBookmark("bm-cancel", "pending", nil)

	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		kb.ContinueWaitForTags(ctx, &KarakeepBookmark{
			Id:            "bm-cancel",
			TaggingStatus: ptrStatus("pending"),
		}, func(updated *KarakeepBookmark) {
			// Should not be called
			t.Error("callback should not be called after context cancellation")
		})
		close(done)
	}()

	// Cancel the parent context immediately
	cancel()

	// The goroutine should exit within a reasonable time
	select {
	case <-done:
		// Expected: goroutine exited
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for goroutine to exit after context cancellation")
	}
}

// TestContinueWaitForTags_WithEditMessageCaption verifies the full handler
// flow: wait timeout -> background poll -> edit caption.
func TestContinueWaitForTags_WithEditMessageCaption(t *testing.T) {
	t.Parallel()

	// Telegram mock for sendDocument and editMessageCaption
	tg := newTelegramMock(t)

	// Tag server for the continuation wait
	tagSrv := newTagServer(t)

	botAPI, err := tgbotapi.New(testToken, tgbotapi.WithServerURL(tg.server.URL), tgbotapi.WithSkipGetMe())
	if err != nil {
		t.Fatalf("failed to create telegram bot: %v", err)
	}

	kb := &KarakeepBot{
		telegram:     &Telegram{Bot: botAPI},
		karakeep:     &Karakeep{ClientWithResponses: tagSrv.client},
		logger:       logging.New(&config.LoggingConfig{Level: "error", Format: "text", Output: "stderr"}),
		waitInterval: 1,
	}

	msg := TelegramMessage{
		ID:      42,
		Chat:    models.Chat{ID: 12345, Type: models.ChatTypePrivate},
		From:    &models.User{ID: 7, Username: "tester"},
		Caption: "test caption",
		Document: &models.Document{
			FileID: "doc-file-id",
		},
	}

	// Simulate sending the Telegram document (first send with initial tags)
	caption := msg.Caption + "\n\n#telegram"
	msgID, err := kb.telegram.SendDocumentWithCaption(context.Background(), &msg, msg.Document.FileID, caption)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msgID != 1 {
		t.Errorf("expected message ID 1, got %d", msgID)
	}

	// Create a KarakeepBookmark with pending status
	kbBookmark := &KarakeepBookmark{
		Id:            "bm-edit-test",
		TaggingStatus: ptrStatus("pending"),
	}

	// Set the bookmark to success immediately on the server.
	// ContinueWaitForTags will detect this after its 15s initial delay.
	tagSrv.setBookmark(kbBookmark.Id, "success", []karakeep.BookmarkTagsAttachedBy{"golang", "pdf"})

	// Start the continuation wait with an EditMessageCaption callback
	var editCalled bool
	var editCaption string
	kb.ContinueWaitForTags(context.Background(), kbBookmark, func(updated *KarakeepBookmark) {
		editCaption = updated.Hashtags()
		if err := kb.telegram.EditMessageCaption(context.Background(), msg.Chat.ID, msgID, editCaption); err != nil {
			t.Errorf("EditMessageCaption failed: %v", err)
		}
		editCalled = true
	})

	// Wait for the edit to happen (poll happens after 15s initial delay)
	select {
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for tag discovery and caption edit")
	case <-time.After(20 * time.Second):
		// Give it a moment for the poll + callback to complete
	}

	if !editCalled {
		t.Error("expected EditMessageCaption callback to be called")
	}

	edits := tg.edits()
	if len(edits) == 0 {
		t.Error("expected at least one editMessageCaption call")
	} else if edits[0].caption != "#golang #pdf" {
		t.Errorf("expected caption '#golang #pdf', got %q", edits[0].caption)
	}
}

// TestContinueWaitForTags_WithEditMessageText verifies the full handler flow
// for a plain text/URL message: wait timeout -> background poll -> edit text.
func TestContinueWaitForTags_WithEditMessageText(t *testing.T) {
	t.Parallel()

	tg := newTelegramMock(t)
	tagSrv := newTagServer(t)

	botAPI, err := tgbotapi.New(testToken, tgbotapi.WithServerURL(tg.server.URL), tgbotapi.WithSkipGetMe())
	if err != nil {
		t.Fatalf("failed to create telegram bot: %v", err)
	}

	kb := &KarakeepBot{
		telegram:     &Telegram{Bot: botAPI},
		karakeep:     &Karakeep{ClientWithResponses: tagSrv.client},
		logger:       logging.New(&config.LoggingConfig{Level: "error", Format: "text", Output: "stderr"}),
		waitInterval: 1,
	}

	msg := TelegramMessage{
		ID:   42,
		Chat: models.Chat{ID: 12345, Type: models.ChatTypePrivate},
		From: &models.User{ID: 7, Username: "tester"},
		Text: "https://example.com",
	}

	// Simulate sending the text message (first send with initial tags).
	initialText := msg.Text + "\n\n#telegram"
	msgID, err := kb.telegram.SendNewMessage(context.Background(), &TelegramMessage{
		ID:   msg.ID,
		Chat: msg.Chat,
		From: msg.From,
		Text: initialText,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msgID != 1 {
		t.Errorf("expected message ID 1, got %d", msgID)
	}

	// Bookmark with pending status, no title, summarization success with a
	// summary (so the title update path is exercised).
	tagSrv.setBookmarkFull("bm-text-edit", "pending", "success", ptrString(""), nil, nil)

	kbBookmark := &KarakeepBookmark{
		Id:            "bm-text-edit",
		TaggingStatus: ptrStatus("pending"),
	}

	// Replicate the handler's callback: edit text + ensure title.
	var editCalled bool
	initialHashtags := "#telegram"
	kb.ContinueWaitForTags(context.Background(), kbBookmark, func(updated *KarakeepBookmark) {
		bgCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		updatedHashtags := updated.Hashtags()
		if updatedHashtags != "" && updatedHashtags != initialHashtags {
			text := msg.Text + "\n\n" + updatedHashtags
			if err := kb.telegram.EditMessageText(bgCtx, msg.Chat.ID, msgID, text); err != nil {
				t.Errorf("EditMessageText failed: %v", err)
			}
			editCalled = true
		}
		kb.ensureBookmarkTitle(bgCtx, updated)
	})

	// Set tags ready after the 15s initial delay so the poll discovers them.
	time.Sleep(16 * time.Second)
	tagSrv.setBookmarkFull("bm-text-edit", "success", "success", ptrString(""), nil, []karakeep.BookmarkTagsAttachedBy{"ai"})

	// Wait for the edit to happen.
	select {
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for tag discovery and text edit")
	case <-time.After(20 * time.Second):
	}

	if !editCalled {
		t.Error("expected EditMessageText callback to be called")
	}

	edits := tg.edits()
	foundText := false
	for _, e := range edits {
		if e.isText && e.text == "https://example.com\n\n#ai" {
			foundText = true
		}
	}
	if !foundText {
		t.Errorf("expected an editMessageText call with the AI tags, got %v", edits)
	}
}

// TestUpdateBookmarkTitle_FromSummary verifies that UpdateBookmarkTitle sends a
// PATCH with both the new title and the existing summary (summary must be
// preserved, not nulled).
func TestUpdateBookmarkTitle_FromSummary(t *testing.T) {
	t.Parallel()

	tagSrv := newTagServer(t)
	kb := waitForBookmark(t, tagSrv)

	// Bookmark with a summary and no title.
	summary := "This is a great article about Go. It covers concurrency."
	tagSrv.setBookmarkFull("bm-title-1", "success", "success", &summary, nil, nil)

	bookmark := &KarakeepBookmark{
		Id:            "bm-title-1",
		TaggingStatus: ptrStatus("success"),
		Summary:       &summary,
	}

	newTitle := bookmark.GenerateTitleFromAI(120)
	if newTitle == "" {
		t.Fatal("expected a non-empty title from summary")
	}

	if err := kb.karakeep.UpdateBookmarkTitle(context.Background(), bookmark, newTitle); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the PATCH payload preserved the summary alongside the title.
	body, ok := tagSrv.patchCall("bm-title-1")
	if !ok {
		t.Fatal("expected a PATCH call to be recorded")
	}

	var title string
	if raw, ok := body["title"]; !ok || string(raw) == "null" {
		t.Error("expected a non-null title in PATCH payload")
	} else {
		_ = json.Unmarshal(raw, &title)
		if title != newTitle {
			t.Errorf("expected title %q, got %q", newTitle, title)
		}
	}

	// Summary must be preserved (not null).
	if raw, ok := body["summary"]; !ok || string(raw) == "null" {
		t.Error("expected summary to be preserved (non-null) in PATCH payload")
	} else {
		var gotSummary string
		_ = json.Unmarshal(raw, &gotSummary)
		if gotSummary != summary {
			t.Errorf("expected summary %q, got %q", summary, gotSummary)
		}
	}
}

// TestUpdateBookmarkTitle_FallbackToAItags verifies that when the summary is
// empty, GenerateTitleFromAI falls back to AI-attached tags.
func TestUpdateBookmarkTitle_FallbackToAItags(t *testing.T) {
	t.Parallel()

	tagSrv := newTagServer(t)
	kb := waitForBookmark(t, tagSrv)

	// Bookmark with empty summary but AI tags.
	tagSrv.setBookmarkFull("bm-title-2", "success", "success", ptrString(""), nil, []karakeep.BookmarkTagsAttachedBy{"ai"})

	bookmark := &KarakeepBookmark{
		Id:            "bm-title-2",
		TaggingStatus: ptrStatus("success"),
		Summary:       ptrString(""),
		Tags: []struct {
			AttachedBy karakeep.BookmarkTagsAttachedBy `json:"attachedBy"`
			Id         string                          `json:"id"`
			Name       string                          `json:"name"`
		}{
			{AttachedBy: karakeep.Ai, Name: "golang"},
			{AttachedBy: karakeep.Ai, Name: "pdf"},
			{AttachedBy: karakeep.Human, Name: "manual"},
		},
	}

	newTitle := bookmark.GenerateTitleFromAI(120)
	if newTitle != "golang, pdf" {
		t.Fatalf("expected 'golang, pdf', got %q", newTitle)
	}

	if err := kb.karakeep.UpdateBookmarkTitle(context.Background(), bookmark, newTitle); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	body, ok := tagSrv.patchCall("bm-title-2")
	if !ok {
		t.Fatal("expected a PATCH call to be recorded")
	}
	var title string
	if raw, ok := body["title"]; ok {
		_ = json.Unmarshal(raw, &title)
	}
	if title != "golang, pdf" {
		t.Errorf("expected title 'golang, pdf', got %q", title)
	}
}

// TestEnsureBookmarkTitle_NoTitle verifies ensureBookmarkTitle is a no-op when
// the bookmark already has a title.
func TestEnsureBookmarkTitle_NoTitle(t *testing.T) {
	t.Parallel()

	tagSrv := newTagServer(t)
	kb := waitForBookmark(t, tagSrv)

	existingTitle := "Existing Title"
	tagSrv.setBookmarkFull("bm-title-3", "success", "success", ptrString("sum"), &existingTitle, nil)

	bookmark := &KarakeepBookmark{
		Id:            "bm-title-3",
		TaggingStatus: ptrStatus("success"),
		Title:         &existingTitle,
	}

	// Should be a no-op (no PATCH call).
	kb.ensureBookmarkTitle(context.Background(), bookmark)

	if _, ok := tagSrv.patchCall("bm-title-3"); ok {
		t.Error("expected no PATCH call when bookmark already has a title")
	}
}

// TestGenerateTitleFromAI_FirstSentence verifies first-sentence extraction and
// rune-based truncation.
func TestGenerateTitleFromAI_FirstSentence(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		summary string
		max     int
		want    string
	}{
		{"single sentence", "Hello world.", 120, "Hello world."},
		{"first of many", "First sentence. Second sentence. Third.", 120, "First sentence."},
		{"no punctuation", "No punctuation here", 120, "No punctuation here"},
		{"truncate", "A very long sentence that goes on and on without any punctuation at all to force truncation", 20, "A very long sente..."},
		{"empty", "", 120, ""},
		{"question", "Is this right? Yes it is.", 120, "Is this right?"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bm := KarakeepBookmark{Summary: &tc.summary}
			got := bm.GenerateTitleFromAI(tc.max)
			if got != tc.want {
				t.Errorf("GenerateTitleFromAI(%q, %d) = %q, want %q", tc.summary, tc.max, got, tc.want)
			}
		})
	}
}

// TestHasTitle verifies HasTitle for nil, empty, and set titles.
func TestHasTitle(t *testing.T) {
	t.Parallel()

	if (KarakeepBookmark{}).HasTitle() {
		t.Error("expected HasTitle=false for nil title")
	}

	empty := ""
	if (KarakeepBookmark{Title: &empty}).HasTitle() {
		t.Error("expected HasTitle=false for empty title")
	}

	set := "  "
	if (KarakeepBookmark{Title: &set}).HasTitle() {
		t.Error("expected HasTitle=false for whitespace-only title")
	}

	title := "Real Title"
	if !(KarakeepBookmark{Title: &title}).HasTitle() {
		t.Error("expected HasTitle=true for set title")
	}
}

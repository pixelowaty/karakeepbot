package karakeepbot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	karakeep "github.com/Madh93/go-karakeep"
	tgbotapi "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/Madh93/karakeepbot/internal/config"
	"github.com/Madh93/karakeepbot/internal/fileprocessor"
	"github.com/Madh93/karakeepbot/internal/filevalidator"
	"github.com/Madh93/karakeepbot/internal/logging"
)

// testPDF is a minimal PDF document (86 bytes) used by the integration tests.
const testPDF = "%PDF-1.4\n% test pdf\n1 0 obj\n<< /Type /Catalog >>\nendobj\ntrailer\n<< /Root 1 0 R >>\n%%EOF\n"

const testToken = "test-token"

// telegramMock is a fake Telegram Bot API server. It answers getFile and
// sendMessage calls in the wire format expected by the tgbotapi library and
// serves file downloads at the URL produced by Bot.FileDownloadLink.
type telegramMock struct {
	server     *httptest.Server
	filePath   string
	fileBody   []byte
	getFileErr bool
	file404    bool

	mu                sync.Mutex
	getFileCallsCount int
	sentReplies       []string
}

func newTelegramMock(t *testing.T) *telegramMock {
	t.Helper()

	m := &telegramMock{
		filePath: "docs/report.pdf",
		fileBody: []byte(testPDF),
	}
	m.server = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.server.Close)

	return m
}

func (m *telegramMock) handle(w http.ResponseWriter, r *http.Request) {
	// API calls are multipart POST requests to /bot{token}/{method}.
	if strings.HasPrefix(r.URL.Path, "/bot"+testToken+"/") {
		method := strings.TrimPrefix(r.URL.Path, "/bot"+testToken+"/")

		switch method {
		case "getFile":
			m.mu.Lock()
			m.getFileCallsCount++
			fail := m.getFileErr
			m.mu.Unlock()

			if fail {
				_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: file not found"}`))
				return
			}
			_, _ = fmt.Fprintf(w, `{"ok":true,"result":{"file_id":"f1","file_unique_id":"u1","file_path":"%s"}}`, m.filePath)
			return

		case "sendMessage":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			text := r.FormValue("text")
			m.mu.Lock()
			m.sentReplies = append(m.sentReplies, text)
			m.mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1,"chat":{"id":1}}}`))
			return

		default:
			_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
		}
	}

	// File downloads are GET requests to /file/bot{token}/{filePath}.
	if strings.HasPrefix(r.URL.Path, "/file/bot"+testToken+"/") {
		m.mu.Lock()
		notFound := m.file404
		body := m.fileBody
		m.mu.Unlock()

		if notFound {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
		return
	}

	http.NotFound(w, r)
}

func (m *telegramMock) getFileCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.getFileCallsCount
}

func (m *telegramMock) replies() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]string(nil), m.sentReplies...)
}

// karakeepMock is a fake Karakeep API server. It receives the multipart asset
// upload and records the received filename, part content type and body.
type karakeepMock struct {
	server    *httptest.Server
	client    *karakeep.ClientWithResponses
	uploadErr bool

	mu               sync.Mutex
	uploadCallsCount int
	fileName         string
	partType         string
	reqType          string
	body             []byte
}

func newKarakeepMock(t *testing.T) *karakeepMock {
	t.Helper()

	m := &karakeepMock{}
	m.server = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.server.Close)

	client, err := karakeep.NewClientWithResponses(m.server.URL + "/api/v1")
	if err != nil {
		t.Fatalf("failed to create karakeep client: %v", err)
	}
	m.client = client

	return m
}

func (m *karakeepMock) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/api/v1/assets" {
		http.NotFound(w, r)
		return
	}

	m.mu.Lock()
	m.uploadCallsCount++
	fail := m.uploadErr
	m.mu.Unlock()

	if fail {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`internal error`))
		return
	}

	if err := r.ParseMultipartForm(1 << 20); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file part: "+err.Error(), http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(file)
	_ = file.Close()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	m.mu.Lock()
	m.fileName = header.Filename
	m.partType = header.Header.Get("Content-Type")
	m.reqType = r.Header.Get("Content-Type")
	m.body = body
	m.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"assetId":"asset-123","fileName":%q,"contentType":%q,"size":%d}`, m.fileName, m.partType, len(body))
}

func (m *karakeepMock) uploadCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.uploadCallsCount
}

func (m *karakeepMock) snapshot() (fileName, partType, reqType string, body []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.fileName, m.partType, m.reqType, m.body
}

// newTestKarakeepBot assembles a KarakeepBot wired to the mock servers. It
// returns the bot and the path of the temporary directory used for downloads.
func newTestKarakeepBot(t *testing.T, tg *telegramMock, kk *karakeepMock, maxsize int64) (*KarakeepBot, string) {
	t.Helper()

	botAPI, err := tgbotapi.New(testToken, tgbotapi.WithServerURL(tg.server.URL), tgbotapi.WithSkipGetMe())
	if err != nil {
		t.Fatalf("failed to create telegram bot: %v", err)
	}

	tempdir := t.TempDir()
	fileProcessor, err := fileprocessor.New(&config.FileProcessorConfig{
		Tempdir: tempdir,
		Maxsize: maxsize,
		Timeout: 5,
	}, false, "")
	if err != nil {
		t.Fatalf("failed to create file processor: %v", err)
	}

	return &KarakeepBot{
		telegram:       &Telegram{Bot: botAPI},
		karakeep:       &Karakeep{ClientWithResponses: kk.client},
		fileProcessor:  fileProcessor,
		fileValidators: map[string]fileprocessor.Validator{"application/pdf": filevalidator.PDFValidator},
		maxFilesize:    maxsize,
		logger:         logging.New(&config.LoggingConfig{Level: "error", Format: "text", Output: "stderr"}),
	}, tempdir
}

func newTestDocumentMessage(fileName, mimeType string, fileSize int64, caption string) TelegramMessage {
	return TelegramMessage{
		ID:      42,
		Chat:    models.Chat{ID: 1, Type: models.ChatTypePrivate},
		From:    &models.User{ID: 7, Username: "tester"},
		Caption: caption,
		Document: &models.Document{
			FileID:   "test-file-id",
			FileName: fileName,
			MimeType: mimeType,
			FileSize: fileSize,
		},
	}
}

func TestSanitizeFileName(t *testing.T) {
	for _, tc := range []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "empty input",
			input:    "",
			expected: "document.pdf",
		},
		{
			name:     "whitespace input",
			input:    "   ",
			expected: "document.pdf",
		},
		{
			name:     "dot",
			input:    ".",
			expected: "document.pdf",
		},
		{
			name:     "dot dot",
			input:    "..",
			expected: "document.pdf",
		},
		{
			name:     "slash",
			input:    "/",
			expected: "document.pdf",
		},
		{
			name:     "plain name",
			input:    "report.pdf",
			expected: "report.pdf",
		},
		{
			name:     "path is reduced to base name",
			input:    "/tmp/report.pdf",
			expected: "report.pdf",
		},
		{
			name:     "quote and newline are replaced",
			input:    "a\"b\r\nc.pdf",
			expected: "a_b__c.pdf",
		},
		{
			name:     "backslash is replaced",
			input:    "a\\b.pdf",
			expected: "a_b.pdf",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := sanitizeFileName(tc.input); got != tc.expected {
				t.Errorf("expected %s, got %s", tc.expected, got)
			}
		})
	}
}

// assertTempDirEmpty fails the test if the temporary directory still holds files.
func assertTempDirEmpty(t *testing.T, tempdir string) {
	t.Helper()

	entries, err := os.ReadDir(tempdir)
	if err != nil {
		t.Fatalf("failed to read temp dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected temp dir to be empty, found %d file(s)", len(entries))
	}
}

func TestHandleDocumentMessage_Success(t *testing.T) {
	t.Parallel()

	pdf := []byte(testPDF)
	tg := newTelegramMock(t)
	tg.fileBody = pdf
	kk := newKarakeepMock(t)

	kb, tempdir := newTestKarakeepBot(t, tg, kk, 100)
	msg := newTestDocumentMessage("report.pdf", "application/pdf", int64(len(pdf)), "my caption")

	bookmark, err := kb.handleDocumentMessage(context.Background(), msg)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}

	ab, ok := bookmark.(*AssetBookmark)
	if !ok {
		t.Fatalf("expected *AssetBookmark, got %T", bookmark)
	}
	if ab.AssetID != "asset-123" {
		t.Errorf("expected asset id asset-123, got %s", ab.AssetID)
	}
	if ab.AssetType != PDFAssetType {
		t.Errorf("expected asset type %s, got %s", PDFAssetType, ab.AssetType)
	}
	if expectedNote := "my caption\n\n" + msg.ContextNote(); ab.Note != expectedNote {
		t.Errorf("unexpected note: %q", ab.Note)
	}

	fileName, partType, reqType, body := kk.snapshot()
	if kk.uploadCalls() != 1 {
		t.Errorf("expected 1 upload, got %d", kk.uploadCalls())
	}
	if fileName != "report.pdf" {
		t.Errorf("expected uploaded filename report.pdf, got %s", fileName)
	}
	if partType != "application/pdf" {
		t.Errorf("expected part content type application/pdf, got %s", partType)
	}
	if !strings.HasPrefix(reqType, "multipart/form-data") {
		t.Errorf("expected multipart/form-data request, got %s", reqType)
	}
	if !bytes.Equal(body, pdf) {
		t.Errorf("uploaded body does not match the original PDF")
	}

	if replies := tg.replies(); len(replies) != 0 {
		t.Errorf("expected no replies on success, got %v", replies)
	}

	assertTempDirEmpty(t, tempdir)
}

func TestHandleDocumentMessage_UnsupportedMimeType(t *testing.T) {
	for _, mimeType := range []string{"application/msword", "image/jpeg"} {
		t.Run(mimeType, func(t *testing.T) {
			t.Parallel()

			tg := newTelegramMock(t)
			kk := newKarakeepMock(t)

			kb, tempdir := newTestKarakeepBot(t, tg, kk, 100)
			msg := newTestDocumentMessage("report.docx", mimeType, 10, "")

			bookmark, err := kb.handleDocumentMessage(context.Background(), msg)
			if err == nil || !strings.Contains(err.Error(), "unsupported document type") {
				t.Fatalf("expected unsupported document type error, got: %v", err)
			}
			if bookmark != nil {
				t.Errorf("expected nil bookmark, got %T", bookmark)
			}

			replies := tg.replies()
			if len(replies) != 1 || replies[0] != "⚠️ Unsupported document type. Only PDF documents are supported." {
				t.Errorf("unexpected replies: %v", replies)
			}
			if tg.getFileCalls() != 0 {
				t.Errorf("expected no getFile calls, got %d", tg.getFileCalls())
			}
			if kk.uploadCalls() != 0 {
				t.Errorf("expected no uploads, got %d", kk.uploadCalls())
			}

			assertTempDirEmpty(t, tempdir)
		})
	}
}

func TestHandleDocumentMessage_DeclaredSizeExceedsLimit(t *testing.T) {
	t.Parallel()

	tg := newTelegramMock(t)
	kk := newKarakeepMock(t)

	kb, tempdir := newTestKarakeepBot(t, tg, kk, 100)
	msg := newTestDocumentMessage("report.pdf", "application/pdf", 101, "")

	bookmark, err := kb.handleDocumentMessage(context.Background(), msg)
	if err == nil || !strings.Contains(err.Error(), "exceeds the maximum file size") {
		t.Fatalf("expected max size error, got: %v", err)
	}
	if bookmark != nil {
		t.Errorf("expected nil bookmark, got %T", bookmark)
	}

	replies := tg.replies()
	if len(replies) != 1 || replies[0] != "⚠️ Document exceeds the maximum file size" {
		t.Errorf("unexpected replies: %v", replies)
	}
	if tg.getFileCalls() != 0 {
		t.Errorf("expected no getFile calls for an oversized document, got %d", tg.getFileCalls())
	}
	if kk.uploadCalls() != 0 {
		t.Errorf("expected no uploads, got %d", kk.uploadCalls())
	}

	assertTempDirEmpty(t, tempdir)
}

func TestHandleDocumentMessage_DownloadedSizeExceedsLimit(t *testing.T) {
	t.Parallel()

	// The declared size fits the limit, but the actual download exceeds it.
	oversized := append([]byte(testPDF), bytes.Repeat([]byte("x"), 60)...)
	tg := newTelegramMock(t)
	tg.fileBody = oversized
	kk := newKarakeepMock(t)

	kb, tempdir := newTestKarakeepBot(t, tg, kk, 100)
	msg := newTestDocumentMessage("report.pdf", "application/pdf", 50, "")

	_, err := kb.handleDocumentMessage(context.Background(), msg)
	if err == nil || !errors.Is(err, fileprocessor.ErrMaxsizeExceeded) {
		t.Fatalf("expected ErrMaxsizeExceeded, got: %v", err)
	}

	replies := tg.replies()
	if len(replies) != 1 || replies[0] != "⚠️ Document exceeds the maximum file size" {
		t.Errorf("unexpected replies: %v", replies)
	}
	if kk.uploadCalls() != 0 {
		t.Errorf("expected no uploads, got %d", kk.uploadCalls())
	}

	assertTempDirEmpty(t, tempdir)
}

func TestHandleDocumentMessage_InvalidPDFSignature(t *testing.T) {
	t.Parallel()

	plainText := []byte("this is not a pdf, just plain text content\n")
	tg := newTelegramMock(t)
	tg.fileBody = plainText
	kk := newKarakeepMock(t)

	kb, tempdir := newTestKarakeepBot(t, tg, kk, 100)
	msg := newTestDocumentMessage("report.pdf", "application/pdf", int64(len(plainText)), "")

	_, err := kb.handleDocumentMessage(context.Background(), msg)
	if err == nil || !errors.Is(err, fileprocessor.ErrValidationFailed) {
		t.Fatalf("expected ErrValidationFailed, got: %v", err)
	}

	replies := tg.replies()
	if len(replies) != 1 || replies[0] != "⚠️ Document is not a valid PDF file" {
		t.Errorf("unexpected replies: %v", replies)
	}
	if kk.uploadCalls() != 0 {
		t.Errorf("expected no uploads, got %d", kk.uploadCalls())
	}

	assertTempDirEmpty(t, tempdir)
}

func TestHandleDocumentMessage_GetFileError(t *testing.T) {
	t.Parallel()

	tg := newTelegramMock(t)
	tg.getFileErr = true
	kk := newKarakeepMock(t)

	kb, tempdir := newTestKarakeepBot(t, tg, kk, 100)
	msg := newTestDocumentMessage("report.pdf", "application/pdf", int64(len(testPDF)), "")

	bookmark, err := kb.handleDocumentMessage(context.Background(), msg)
	if err == nil || !strings.Contains(err.Error(), "couldn't get file URL") {
		t.Fatalf("expected file URL error, got: %v", err)
	}
	if bookmark != nil {
		t.Errorf("expected nil bookmark, got %T", bookmark)
	}

	replies := tg.replies()
	if len(replies) != 1 || replies[0] != "⚠️ Failed to process document from Telegram servers, try again later" {
		t.Errorf("unexpected replies: %v", replies)
	}
	if kk.uploadCalls() != 0 {
		t.Errorf("expected no uploads, got %d", kk.uploadCalls())
	}

	assertTempDirEmpty(t, tempdir)
}

func TestHandleDocumentMessage_DownloadNotFound(t *testing.T) {
	t.Parallel()

	tg := newTelegramMock(t)
	tg.file404 = true
	kk := newKarakeepMock(t)

	kb, tempdir := newTestKarakeepBot(t, tg, kk, 100)
	msg := newTestDocumentMessage("report.pdf", "application/pdf", int64(len(testPDF)), "")

	_, err := kb.handleDocumentMessage(context.Background(), msg)
	if err == nil || !errors.Is(err, fileprocessor.ErrDownloadFailed) {
		t.Fatalf("expected ErrDownloadFailed, got: %v", err)
	}

	replies := tg.replies()
	if len(replies) != 1 || replies[0] != "⚠️ Failed to process document" {
		t.Errorf("unexpected replies: %v", replies)
	}
	if kk.uploadCalls() != 0 {
		t.Errorf("expected no uploads, got %d", kk.uploadCalls())
	}

	assertTempDirEmpty(t, tempdir)
}

func TestHandleDocumentMessage_UploadError(t *testing.T) {
	t.Parallel()

	tg := newTelegramMock(t)
	kk := newKarakeepMock(t)
	kk.uploadErr = true

	kb, tempdir := newTestKarakeepBot(t, tg, kk, 100)
	msg := newTestDocumentMessage("report.pdf", "application/pdf", int64(len(testPDF)), "")

	bookmark, err := kb.handleDocumentMessage(context.Background(), msg)
	if err == nil || !strings.Contains(err.Error(), "couldn't upload asset") {
		t.Fatalf("expected upload error, got: %v", err)
	}
	if bookmark != nil {
		t.Errorf("expected nil bookmark, got %T", bookmark)
	}

	replies := tg.replies()
	if len(replies) != 1 || replies[0] != "⚠️ Failed to upload asset to Karakeep" {
		t.Errorf("unexpected replies: %v", replies)
	}
	if kk.uploadCalls() != 1 {
		t.Errorf("expected 1 upload attempt, got %d", kk.uploadCalls())
	}

	assertTempDirEmpty(t, tempdir)
}

func TestHandleDocumentMessage_FallbackFileName(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fileName string
		expected string
	}{
		{
			name:     "empty file name",
			fileName: "",
			expected: "document.pdf",
		},
		{
			name:     "whitespace file name",
			fileName: "   ",
			expected: "document.pdf",
		},
		{
			name:     "plain file name is preserved",
			fileName: "report.pdf",
			expected: "report.pdf",
		},
		{
			name:     "path is reduced to base name",
			fileName: "/tmp/report.pdf",
			expected: "report.pdf",
		},
		{
			name:     "unsafe characters are replaced",
			fileName: "report\"evil\r\n.pdf",
			expected: "report_evil__.pdf",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tg := newTelegramMock(t)
			kk := newKarakeepMock(t)

			kb, tempdir := newTestKarakeepBot(t, tg, kk, 100)
			msg := newTestDocumentMessage(tc.fileName, "application/pdf", int64(len(testPDF)), "")

			bookmark, err := kb.handleDocumentMessage(context.Background(), msg)
			if err != nil {
				t.Fatalf("expected success, got error: %v", err)
			}
			if _, ok := bookmark.(*AssetBookmark); !ok {
				t.Fatalf("expected *AssetBookmark, got %T", bookmark)
			}

			fileName, _, _, _ := kk.snapshot()
			if fileName != tc.expected {
				t.Errorf("expected uploaded filename %s, got %s", tc.expected, fileName)
			}

			assertTempDirEmpty(t, tempdir)
		})
	}
}

// Package karakeepbot implements a Telegram bot that allows users to create
// bookmarks through messages. The bot interacts with the Karakeep API to manage
// bookmarks and handles incoming messages by checking if the chat ID is
// allowed, creating bookmarks, and sending back updated messages with tags.
package karakeepbot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Madh93/go-karakeep"
	"github.com/Madh93/karakeepbot/internal/config"
	"github.com/Madh93/karakeepbot/internal/fileprocessor"
	"github.com/Madh93/karakeepbot/internal/filevalidator"
	"github.com/Madh93/karakeepbot/internal/logging"
	"github.com/Madh93/karakeepbot/internal/validation"
)

// maxTagRetries is the number of times to wait for Karakeep AI tagging to
// complete before proceeding. At 5s per retry, 6 retries = ~30s timeout.
const maxTagRetries = 6

const (
	defaultBgPollInterval = 15 * time.Second
	defaultBgPollTimeout  = 5 * time.Minute
)

// KarakeepBot represents the bot with its dependencies, including the Karakeep
// client, Telegram bot, logger and other options.
type KarakeepBot struct {
	karakeep       *Karakeep
	telegram       *Telegram
	logger         *logging.Logger
	fileProcessor  *fileprocessor.Processor
	fileValidators map[string]fileprocessor.Validator
	maxFilesize    int64
	allowlist      []int64
	threads        []int
	waitInterval   int
	bgPollInterval time.Duration
	bgPollTimeout  time.Duration
}

func (kb KarakeepBot) getBgPollInterval() time.Duration {
	if kb.bgPollInterval > 0 {
		return kb.bgPollInterval
	}
	return defaultBgPollInterval
}

func (kb KarakeepBot) getBgPollTimeout() time.Duration {
	if kb.bgPollTimeout > 0 {
		return kb.bgPollTimeout
	}
	return defaultBgPollTimeout
}

// formatWithHashtags appends hashtags to base separated by a blank line.
// If either base or hashtags is empty, the non-empty part is returned as-is.
func formatWithHashtags(base, hashtags string) string {
	if hashtags == "" {
		return base
	}
	if base == "" {
		return hashtags
	}
	return base + "\n\n" + hashtags
}

// New creates a new KarakeepBot instance, initializing the Karakeep and Telegram
// clients.
func New(logger *logging.Logger, config *config.Config) *KarakeepBot {
	// Initialize FileProcessor
	fileProcessor, err := fileprocessor.New(&config.FileProcessor, config.Telegram.ProxyEnabled, config.Telegram.ProxyURL)
	if err != nil {
		logger.Fatal("Failed to create file processor", "error", err)
	}

	// Setup Supported File Validators
	fileValidators := make(map[string]fileprocessor.Validator)
	fileValidators["image/jpeg"] = filevalidator.ImageValidator
	fileValidators["image/png"] = filevalidator.ImageValidator
	fileValidators["image/webp"] = filevalidator.ImageValidator
	fileValidators["application/pdf"] = filevalidator.PDFValidator

	// Check if the validators passed in the configuration are supported
	if len(config.FileProcessor.Mimetypes) > 0 {
		for _, mimetype := range config.FileProcessor.Mimetypes {
			if _, supported := fileValidators[mimetype]; !supported {
				logger.Fatal("Configuration error: unsupported MIME type configured", "mime_type", mimetype)
			}
		}
	}

	return &KarakeepBot{
		karakeep:       createKarakeep(logger, &config.Karakeep),
		telegram:       createTelegram(logger, &config.Telegram),
		allowlist:      config.Telegram.Allowlist,
		threads:        config.Telegram.Threads,
		waitInterval:   config.Karakeep.Interval,
		fileProcessor:  fileProcessor,
		fileValidators: fileValidators,
		maxFilesize:    config.FileProcessor.Maxsize,
		logger:         logger,
	}
}

// Run starts the bot and handles incoming messages.
func (kb *KarakeepBot) Run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	// Set default handler
	kb.telegram.RegisterHandlerMatchFunc(func(*TelegramUpdate) bool { return true }, kb.handler)

	// Start the bot
	kb.telegram.Start(ctx)

	return nil
}

// handler is the main handler for incoming messages. It processes the message
// and sends a response back to the user.
func (kb KarakeepBot) handler(ctx context.Context, _ *Bot, update *TelegramUpdate) {
	if update.Message == nil {
		return
	}

	msg := TelegramMessage(*update.Message)

	// Check if the chat ID is allowed
	if !kb.isChatIdAllowed(msg.Chat.ID) {
		kb.logger.Warn(fmt.Sprintf("Received message from not allowed chat ID. Allowed chats IDs: %v", kb.allowlist), msg.Attrs()...)
		return
	}

	// Check if the thread ID is allowed
	if !kb.isThreadIdAllowed(msg.MessageThreadID) {
		kb.logger.Warn(fmt.Sprintf("Received message from not allowed thread ID. Allowed thread IDs: %v", kb.threads), msg.Attrs()...)
		return
	}

	kb.logger.Debug("Received message from allowed chat ID and allowed thread ID", msg.Attrs()...)

	// Parse the message to get corresponding bookmark type
	kb.logger.Debug("Parsing message to get corresponding bookmark type", msg.Attrs()...)
	b, err := kb.parseMessage(ctx, msg)
	if err != nil {
		kb.logger.Error("Failed to parse message", msg.AttrsWithError(err)...)
		return
	}

	// Create the bookmark
	kb.logger.Debug(fmt.Sprintf("Creating bookmark of type %s", b))
	bookmark, err := kb.karakeep.CreateBookmark(ctx, b)
	if err != nil {
		kb.logger.Error("Failed to create bookmark", "error", err)
		return
	}
	kb.logger.Info("Created bookmark", bookmark.Attrs()...)

	// Enrich bookmark with Telegram origin metadata
	kb.logger.Debug("Enriching bookmark with Telegram origin metadata", bookmark.Attrs()...)
	kb.enrichBookmark(ctx, msg, bookmark)

	// Wait until bookmark tags are updated (with a timeout to avoid hanging on
	// uncrawlable URLs)
	kb.logger.Debug("Waiting for bookmark tags to be updated", bookmark.Attrs()...)
	result, err := kb.waitForTagCompletion(ctx, bookmark)
	if err != nil {
		kb.logger.Error("Failed to wait for bookmark tagging", "error", err)
		return
	}

	bookmark = result.Bookmark
	hashtags := bookmark.Hashtags()
	originalText := msg.Text
	var newMessageID int
	// isMedia is true when the sent message is a photo/document with a
	// caption (edited via editMessageCaption); false for plain text/URL
	// messages (edited via editMessageText).
	isMedia := false

	// Send back with hashtags
	if msg.Photo != nil {
		caption := formatWithHashtags(msg.Caption, hashtags)

		// Send back the original photo with hashtags as caption
		kb.logger.Debug("Sending updated message with photo and hashtags", msg.Attrs()...)
		newMessageID, err = kb.telegram.SendPhotoWithCaption(ctx, &msg, msg.Photo[len(msg.Photo)-1].FileID, caption)
		if err != nil {
			kb.logger.Error("Failed to send photo with caption", msg.AttrsWithError(err)...)
			return
		}
		isMedia = true
	} else if msg.Document != nil {
		caption := formatWithHashtags(msg.Caption, hashtags)

		// Send back the original document with hashtags as caption
		kb.logger.Debug("Sending updated message with document and hashtags", msg.Attrs()...)
		newMessageID, err = kb.telegram.SendDocumentWithCaption(ctx, &msg, msg.Document.FileID, caption)
		if err != nil {
			kb.logger.Error("Failed to send document with caption", msg.AttrsWithError(err)...)
			return
		}
		isMedia = true
	} else {
		outMsg := msg
		outMsg.Text = formatWithHashtags(originalText, hashtags)

		// Send back new message with hashtags
		kb.logger.Debug("Sending updated message with hashtags", outMsg.Attrs()...)
		newMessageID, err = kb.telegram.SendNewMessage(ctx, &outMsg)
		if err != nil {
			kb.logger.Error("Failed to send new message", outMsg.AttrsWithError(err)...)
			return
		}
	}

	// If tags appeared after the initial timeout, continue polling in the
	// background and update the Telegram message once they arrive.
	if result.TagsUpdatedAfterTimeout && newMessageID > 0 {
		kb.ContinueWaitForTags(ctx, bookmark, func(updated *KarakeepBookmark) {
			// Bound the Telegram edit call while keeping cancellation tied to ctx.
			bgCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()

			// Update the Telegram message with the new tags, skipping no-op
			// edits (Telegram rejects edits that do not change the content).
			updatedHashtags := updated.Hashtags()
			if updatedHashtags != "" && updatedHashtags != hashtags {
				if isMedia {
					caption := formatWithHashtags(msg.Caption, updatedHashtags)
					if err := kb.telegram.EditMessageCaption(bgCtx, msg.Chat.ID, newMessageID, caption); err != nil {
						kb.logger.Error("Failed to update Telegram message caption", "message_id", newMessageID, "error", err)
					} else {
						kb.logger.Info("Updated Telegram message caption with AI tags", "message_id", newMessageID, "tags", updatedHashtags)
					}
				} else {
					text := formatWithHashtags(originalText, updatedHashtags)
					if err := kb.telegram.EditMessageText(bgCtx, msg.Chat.ID, newMessageID, text); err != nil {
						kb.logger.Error("Failed to update Telegram message text", "message_id", newMessageID, "error", err)
					} else {
						kb.logger.Info("Updated Telegram message text with AI tags", "message_id", newMessageID, "tags", updatedHashtags)
					}
				}
			}
		})
	}

	// Delete original message
	kb.logger.Debug("Deleting original message", msg.Attrs()...)
	if err := kb.telegram.DeleteOriginalMessage(ctx, &msg); err != nil {
		kb.logger.Error("Failed to delete original message", msg.AttrsWithError(err)...)
		return
	}

	kb.logger.Info("Updated message", msg.Attrs()...)
}

// isChatIdAllowed checks if the chat ID is allowed to receive messages.
func (kb KarakeepBot) isChatIdAllowed(chatId int64) bool {
	// When no allowlist is provided, all chat IDs are allowed
	if len(kb.allowlist) == 0 {
		return true
	}

	// When the allowlist provided by environment variable is empty, it contains
	// a single element with value 0.
	if len(kb.allowlist) == 1 && kb.allowlist[0] == 0 {
		return true
	}

	return slices.Contains(kb.allowlist, chatId)
}

// isThreadIdAllowed checks if the thread ID is allowed to receive messages.
func (kb KarakeepBot) isThreadIdAllowed(threadId int) bool {
	return len(kb.threads) == 0 || slices.Contains(kb.threads, threadId)
}

// TagCompletionResult holds the result of waiting for bookmark tagging to
// complete.
type TagCompletionResult struct {
	// Bookmark is the bookmark with the latest available tags.
	Bookmark *KarakeepBookmark
	// TagsUpdatedAfterTimeout indicates whether new tags appeared after the
	// initial timeout, meaning the Telegram message should be updated.
	TagsUpdatedAfterTimeout bool
}

// waitForTagCompletion polls the bookmark tagging status until it succeeds,
// fails, or the initial retry timeout is reached. Returns a result containing
// the bookmark and a flag indicating whether tags appeared after the timeout.
func (kb *KarakeepBot) waitForTagCompletion(ctx context.Context, bookmark *KarakeepBookmark) (*TagCompletionResult, error) {
	retries := 0
	for {
		var err error
		bookmark, err = kb.karakeep.RetrieveBookmarkById(ctx, bookmark.Id)
		if err != nil {
			return nil, err
		}
		if bookmark.TaggingStatus == nil {
			return nil, fmt.Errorf("bookmark tagging status is nil")
		}
		if *bookmark.TaggingStatus == karakeep.BookmarkTaggingStatusSuccess {
			return &TagCompletionResult{Bookmark: bookmark, TagsUpdatedAfterTimeout: false}, nil
		}
		if *bookmark.TaggingStatus == karakeep.BookmarkTaggingStatusFailure {
			return nil, fmt.Errorf("bookmark tagging failed")
		}
		retries++
		if retries >= maxTagRetries {
			kb.logger.Warn("Bookmark tagging did not complete within timeout, continuing in background", bookmark.Attrs()...)
			// TagsUpdatedAfterTimeout is true so the caller starts background
			// polling (ContinueWaitForTags) to catch tags that arrive later.
			return &TagCompletionResult{Bookmark: bookmark, TagsUpdatedAfterTimeout: true}, nil
		}
		kb.logger.Debug(fmt.Sprintf("Bookmark is still pending, waiting %d seconds before retrying", kb.waitInterval), bookmark.Attrs()...)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(kb.waitInterval) * time.Second):
		}
	}
}

// ContinueWaitForTags continues polling the Karakeep API for bookmark tagging
// completion in a background goroutine in case the initial wait timed out. It
// calls onUpdate when tagging succeeds with at least one tag, and stops when
// tagging completes (with or without tags), fails, hits consecutive errors, or
// ctx/timeout expires. The returned channel is closed when the goroutine exits.
func (kb *KarakeepBot) ContinueWaitForTags(ctx context.Context, bookmark *KarakeepBookmark, onUpdate func(*KarakeepBookmark)) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)

		pollCtx, cancel := context.WithTimeout(ctx, kb.getBgPollTimeout())
		defer cancel()

		interval := kb.getBgPollInterval()

		// Initial delay so we do not hammer the API right after the
		// timeout-based send. Give Karakeep a bit more time to finish.
		select {
		case <-pollCtx.Done():
			return
		case <-time.After(interval):
		}

		const maxConsecutiveErrors = 3
		consecutiveErrors := 0

		for {
			select {
			case <-pollCtx.Done():
				return
			default:
			}

			updated, err := kb.karakeep.RetrieveBookmarkById(pollCtx, bookmark.Id)
			if err != nil {
				if pollCtx.Err() != nil {
					return
				}
				consecutiveErrors++
				kb.logger.Warn("ContinueWaitForTags: failed to retrieve bookmark", "bookmark_id", bookmark.Id, "attempt", consecutiveErrors, "error", err)
				if consecutiveErrors >= maxConsecutiveErrors {
					kb.logger.Error("ContinueWaitForTags: aborting after consecutive errors", "bookmark_id", bookmark.Id, "error", err)
					return
				}
				select {
				case <-pollCtx.Done():
					return
				case <-time.After(interval):
				}
				continue
			}
			consecutiveErrors = 0

			if updated.TaggingStatus == nil {
				kb.logger.Error("ContinueWaitForTags: bookmark tagging status is nil", "bookmark_id", bookmark.Id)
				return
			}

			if *updated.TaggingStatus == karakeep.BookmarkTaggingStatusFailure {
				kb.logger.Error("ContinueWaitForTags: bookmark tagging failed", "bookmark_id", bookmark.Id)
				return
			}

			if *updated.TaggingStatus == karakeep.BookmarkTaggingStatusSuccess {
				if len(updated.Tags) > 0 {
					kb.logger.Info("Tags discovered during background polling, updating Telegram message", "bookmark_id", bookmark.Id, "tags", len(updated.Tags))
					onUpdate(updated)
				} else {
					kb.logger.Debug("ContinueWaitForTags: bookmark tagging completed with no tags", "bookmark_id", bookmark.Id)
				}
				return
			}

			kb.logger.Debug("ContinueWaitForTags: still waiting for tags", "bookmark_id", bookmark.Id)
			select {
			case <-pollCtx.Done():
				return
			case <-time.After(interval):
			}
		}
	}()
	return done
}

// parseMessage parses the incoming Telegram message and returns the
// corresponding Bookmark type.
func (kb KarakeepBot) parseMessage(ctx context.Context, msg TelegramMessage) (BookmarkType, error) {
	if msg.Photo != nil {
		return kb.handlePhotoMessage(ctx, msg)
	}

	if msg.Document != nil {
		return kb.handleDocumentMessage(ctx, msg)
	}

	if url := msg.ChannelPostLink(); url != "" {
		lb := NewLinkBookmark(url)
		lb.Title = extractTitle(msg.Text)

		var parts []string
		if text := strings.TrimSpace(msg.Text); text != "" {
			parts = append(parts, text)
		}
		if entityURLs := msg.EntityURLs(); len(entityURLs) > 0 {
			parts = append(parts, "Links:\n"+strings.Join(entityURLs, "\n"))
		}
		if ctxNote := msg.ContextNote(); ctxNote != "" {
			parts = append(parts, ctxNote)
		}
		lb.Note = strings.Join(parts, "\n\n")

		return lb, nil
	}

	if url := msg.ExtractURL(); url != "" {
		return newSimpleLinkBookmark(url, msg), nil
	}

	if validation.ValidateURL(msg.Text) == nil {
		return newSimpleLinkBookmark(msg.Text, msg), nil
	}

	if url := extractEmbeddedURL(msg.Text); url != "" {
		return newSimpleLinkBookmark(url, msg), nil
	}

	if msg.Text != "" {
		tb := NewTextBookmark(msg.Text)
		if ctxNote := msg.ContextNote(); ctxNote != "" {
			tb.Note = ctxNote
		}
		return tb, nil
	}

	return nil, errors.New("unsupported bookmark type")
}

// newSimpleLinkBookmark creates a LinkBookmark from a URL and message, with
// title extracted from the first line and a note combining the message text
// with Telegram origin context.
func newSimpleLinkBookmark(url string, msg TelegramMessage) *LinkBookmark {
	lb := NewLinkBookmark(url)
	lb.Title = extractTitle(msg.Text)
	lb.Note = buildNote(msg.Text, msg.ContextNote())
	return lb
}

// extractTitle returns the first line of text as a title, truncated to 150
// characters to avoid Karakeep limits.
func extractTitle(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	lines := strings.SplitN(text, "\n", 2)
	title := strings.TrimSpace(lines[0])
	runes := []rune(title)
	if len(runes) > 150 {
		title = string(runes[:150]) + "..."
	}
	return title
}

// buildNote combines the original message text with a context note.
func buildNote(text, contextNote string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return contextNote
	}
	if contextNote == "" {
		return text
	}
	return text + "\n\n" + contextNote
}

// extractEmbeddedURL finds the first http or https URL within arbitrary text.
// Returns empty string if no URL is found.
func extractEmbeddedURL(text string) string {
	words := strings.Fields(text)
	for _, word := range words {
		word = strings.TrimRight(word, ",.!?:;)]}")
		if err := validation.ValidateURL(word); err == nil {
			return word
		}
	}
	return ""
}

// handlePhotoMessage processes a message containing a photo.
func (kb *KarakeepBot) handlePhotoMessage(ctx context.Context, msg TelegramMessage) (bookmark BookmarkType, err error) {
	// Select the largest photo
	photo := msg.Photo[len(msg.Photo)-1]
	kb.logger.Debug("Handling Telegram image", "file_id", photo.FileID, "file_size", photo.FileSize)

	// Get file URL
	fileURL, err := kb.telegram.GetFileURL(ctx, photo.FileID)
	if err != nil {
		kb.logger.Error("Failed to get file URL", msg.AttrsWithError(err)...)
		if replyErr := kb.telegram.SendReply(ctx, &msg, "⚠️ Failed to process image from Telegram servers, try again later"); replyErr != nil {
			kb.logger.Error("Failed to send reply to user", "reply_error", replyErr)
		}
		return nil, errors.New("couldn't get file URL")
	}

	// Download file. NOTE: Telegram Photo does not have mime type info. We can't use any validator.
	filePath, mimeType, err := kb.fileProcessor.Process(fileURL, nil)
	if err != nil {
		kb.logger.Error("Failed to process image", msg.AttrsWithError(err)...)
		reply := "⚠️ Failed to process image"
		if errors.Is(err, fileprocessor.ErrMaxsizeExceeded) {
			reply = "⚠️ Image exceeds the maximum file size"
		}
		if replyErr := kb.telegram.SendReply(ctx, &msg, reply); replyErr != nil {
			kb.logger.Error("Failed to send reply to user", "reply_error", replyErr)
		}
		return nil, fmt.Errorf("couldn't process image: %w", err)
	}
	defer func() {
		if cleanupErr := kb.fileProcessor.Cleanup(filePath); cleanupErr != nil {
			// A cleanup failure after a successful upload must not turn the
			// result into an error: the file was already uploaded to Karakeep.
			kb.logger.Error("Failed to cleanup temporary file", "path", filePath, "error", cleanupErr)
		}
	}()

	kb.logger.Debug("Detected MIME type", "mime_type", mimeType)

	// Upload asset to Karakeep
	asset, err := kb.karakeep.CreateAsset(ctx, filePath, mimeType)
	if err != nil {
		kb.logger.Error("Failed to upload asset", msg.AttrsWithError(err)...)
		if replyErr := kb.telegram.SendReply(ctx, &msg, "⚠️ Failed to upload asset to Karakeep"); replyErr != nil {
			kb.logger.Error("Failed to send reply to user", "reply_error", replyErr)
		}
		return nil, errors.New("couldn't upload asset")
	}

	kb.logger.Debug("Asset uploaded successfully", "asset_id", asset.AssetId)

	// Get note from caption and append Telegram origin context
	note := strings.TrimSpace(msg.Caption)
	ab := NewAssetBookmark(asset.AssetId, ImageAssetType, note)
	if ctxNote := msg.ContextNote(); ctxNote != "" {
		if ab.Note != "" {
			ab.Note += "\n\n" + ctxNote
		} else {
			ab.Note = ctxNote
		}
	}
	return ab, nil
}

// sanitizeFileName derives a safe base file name from the provided name. It
// strips directory components, replaces characters that are unsafe in a
// multipart header, and falls back to "document.pdf" when no usable name
// remains.
func sanitizeFileName(name string) string {
	fileName := strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r < 0x20 || r == 0x7f {
			return '_'
		}
		return r
	}, name)

	fileName = filepath.Base(fileName)
	if fileName == "." || fileName == ".." || fileName == "/" || strings.TrimSpace(fileName) == "" {
		return "document.pdf"
	}
	return fileName
}

// isPDFDocument reports whether a Telegram document is plausibly a PDF. A PDF
// MIME type always indicates a PDF. Telegram sometimes reports an empty or
// generic (application/octet-stream) MIME type, in which case the file name
// extension is used as a hint. A specific non-PDF MIME type is not overridden
// by the extension, so a file is never accepted on the extension alone. The
// file content is always validated with the PDF validator before the document
// is processed.
func isPDFDocument(mimeType, fileName string) bool {
	mt := strings.ToLower(strings.TrimSpace(mimeType))
	if base, _, ok := strings.Cut(mt, ";"); ok {
		mt = strings.TrimSpace(base)
	}
	if mt == "application/pdf" {
		return true
	}
	switch mt {
	case "", "application/octet-stream":
		return strings.EqualFold(filepath.Ext(fileName), ".pdf")
	}
	return false
}

// handleDocumentMessage processes a message containing a document. Only PDF
// documents are supported.
func (kb *KarakeepBot) handleDocumentMessage(ctx context.Context, msg TelegramMessage) (bookmark BookmarkType, err error) {
	doc := msg.Document
	kb.logger.Debug("Handling Telegram document", "file_id", doc.FileID, "file_name", doc.FileName, "file_size", doc.FileSize, "mime_type", doc.MimeType)

	// Only PDF documents are supported. Telegram may report an empty or generic
	// MIME type, so a PDF file name extension is also accepted; the content is
	// always validated with the PDF validator before processing.
	if !isPDFDocument(doc.MimeType, doc.FileName) {
		kb.logger.Warn("Unsupported document type", append(msg.Attrs(), "mime_type", doc.MimeType)...)
		if replyErr := kb.telegram.SendReply(ctx, &msg, "⚠️ Unsupported document type. Only PDF documents are supported."); replyErr != nil {
			kb.logger.Error("Failed to send reply to user", "reply_error", replyErr)
		}
		return nil, fmt.Errorf("unsupported document type: %s", doc.MimeType)
	}

	validator := kb.fileValidators["application/pdf"]
	if validator == nil {
		validator = filevalidator.PDFValidator
	}

	// Check the declared file size before downloading.
	if kb.maxFilesize > 0 && doc.FileSize > kb.maxFilesize {
		kb.logger.Warn("Document exceeds the maximum file size", append(msg.Attrs(), "file_size", doc.FileSize, "max_size", kb.maxFilesize)...)
		if replyErr := kb.telegram.SendReply(ctx, &msg, "⚠️ Document exceeds the maximum file size"); replyErr != nil {
			kb.logger.Error("Failed to send reply to user", "reply_error", replyErr)
		}
		return nil, fmt.Errorf("document exceeds the maximum file size: %d bytes", doc.FileSize)
	}

	// Get file URL
	fileURL, err := kb.telegram.GetFileURL(ctx, doc.FileID)
	if err != nil {
		kb.logger.Error("Failed to get file URL", msg.AttrsWithError(err)...)
		if replyErr := kb.telegram.SendReply(ctx, &msg, "⚠️ Failed to process document from Telegram servers, try again later"); replyErr != nil {
			kb.logger.Error("Failed to send reply to user", "reply_error", replyErr)
		}
		return nil, errors.New("couldn't get file URL")
	}

	// Download file and validate its content.
	filePath, mimeType, err := kb.fileProcessor.Process(fileURL, validator)
	if err != nil {
		kb.logger.Error("Failed to process document", msg.AttrsWithError(err)...)
		reply := "⚠️ Failed to process document"
		if errors.Is(err, fileprocessor.ErrMaxsizeExceeded) {
			reply = "⚠️ Document exceeds the maximum file size"
		} else if errors.Is(err, fileprocessor.ErrValidationFailed) {
			reply = "⚠️ Document is not a valid PDF file"
		}
		if replyErr := kb.telegram.SendReply(ctx, &msg, reply); replyErr != nil {
			kb.logger.Error("Failed to send reply to user", "reply_error", replyErr)
		}
		return nil, fmt.Errorf("couldn't process document: %w", err)
	}
	defer func() {
		if cleanupErr := kb.fileProcessor.Cleanup(filePath); cleanupErr != nil {
			// A cleanup failure after a successful upload must not turn the
			// result into an error: the file was already uploaded to Karakeep.
			kb.logger.Error("Failed to cleanup temporary file", "path", filePath, "error", cleanupErr)
		}
	}()

	kb.logger.Debug("Detected MIME type", "mime_type", mimeType)

	// Preserve the original file name where possible.
	fileName := sanitizeFileName(doc.FileName)

	// Upload asset to Karakeep
	asset, err := kb.karakeep.CreateAssetWithFileName(ctx, filePath, fileName, mimeType)
	if err != nil {
		kb.logger.Error("Failed to upload asset", msg.AttrsWithError(err)...)
		if replyErr := kb.telegram.SendReply(ctx, &msg, "⚠️ Failed to upload asset to Karakeep"); replyErr != nil {
			kb.logger.Error("Failed to send reply to user", "reply_error", replyErr)
		}
		return nil, errors.New("couldn't upload asset")
	}

	kb.logger.Debug("Asset uploaded successfully", "asset_id", asset.AssetId, "file_name", fileName)

	// Get note from caption and append Telegram origin context
	note := strings.TrimSpace(msg.Caption)
	ab := NewAssetBookmark(asset.AssetId, PDFAssetType, note)
	if ctxNote := msg.ContextNote(); ctxNote != "" {
		if ab.Note != "" {
			ab.Note += "\n\n" + ctxNote
		} else {
			ab.Note = ctxNote
		}
	}
	return ab, nil
}

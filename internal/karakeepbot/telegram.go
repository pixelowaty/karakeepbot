package karakeepbot

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/Madh93/karakeepbot/internal/config"
	"github.com/Madh93/karakeepbot/internal/logging"
	"github.com/Madh93/karakeepbot/internal/secret"
	tgbotapi "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// Bot is an alias for tgbotapi.Bot.
type Bot = tgbotapi.Bot

// Telegram embeds the Telegram bot API client to add high level functionality.
type Telegram struct {
	*Bot
	token secret.String
}

// createTelegram initializes the Telegram Bot API client.
func createTelegram(logger *logging.Logger, config *config.TelegramConfig) *Telegram {
	logger.Debug(fmt.Sprintf("Initializing Telegram Bot API using %s token", config.Token))

	var opts []tgbotapi.Option

	if config.ProxyEnabled {
		proxyURL, err := url.Parse(config.ProxyURL)
		if err != nil {
			logger.Fatal("Error parsing proxy URL.", "error", err)
		}

		transport := &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		}
		client := &http.Client{
			Transport: transport,
		}
		opts = append(opts, tgbotapi.WithHTTPClient(30*time.Second, client))
		opts = append(opts, tgbotapi.WithCheckInitTimeout(30*time.Second))
	}

	telegramBot, err := tgbotapi.New(config.Token.Value(), opts...)
	if err != nil {
		logger.Fatal("Error creating Telegram Bot API.", "error", err)
	}

	return &Telegram{Bot: telegramBot, token: config.Token}
}

// SendNewMessage sends a new message to the user's chat. Returns the ID of
// the newly created message, or 0 on error.
func (t Telegram) SendNewMessage(ctx context.Context, msg *TelegramMessage) (int, error) {
	params := &tgbotapi.SendMessageParams{
		ChatID:          msg.Chat.ID,
		MessageThreadID: msg.MessageThreadID,
		Text:            msg.Text,
	}

	resp, err := t.SendMessage(ctx, params)
	if err != nil {
		return 0, err
	}

	return resp.ID, nil
}

// SendPhotoWithCaption sends a photo with a caption. Returns the ID of the
// newly created message, or 0 on error.
func (t *Telegram) SendPhotoWithCaption(ctx context.Context, msg *TelegramMessage, photoID string, caption string) (int, error) {
	params := &tgbotapi.SendPhotoParams{
		ChatID:          msg.Chat.ID,
		MessageThreadID: msg.MessageThreadID,
		Photo:           &models.InputFileString{Data: photoID},
		Caption:         caption,
	}

	resp, err := t.SendPhoto(ctx, params)
	if err != nil {
		return 0, err
	}

	return resp.ID, nil
}

// SendDocumentWithCaption sends a document with a caption. Returns the ID of
// the newly created message, or 0 on error.
func (t *Telegram) SendDocumentWithCaption(ctx context.Context, msg *TelegramMessage, documentID string, caption string) (int, error) {
	params := &tgbotapi.SendDocumentParams{
		ChatID:          msg.Chat.ID,
		MessageThreadID: msg.MessageThreadID,
		Document:        &models.InputFileString{Data: documentID},
		Caption:         caption,
	}

	resp, err := t.SendDocument(ctx, params)
	if err != nil {
		return 0, err
	}

	return resp.ID, nil
}

// SendReply sends a reply to a specific message.
func (t Telegram) SendReply(ctx context.Context, msg *TelegramMessage, text string) error {
	params := &tgbotapi.SendMessageParams{
		ChatID:          msg.Chat.ID,
		MessageThreadID: msg.MessageThreadID,
		ReplyParameters: &models.ReplyParameters{MessageID: msg.ID},
		Text:            text,
	}

	if _, err := t.SendMessage(ctx, params); err != nil {
		return err
	}

	return nil
}

// EditMessageCaption edits the caption of an existing message.
func (t Telegram) EditMessageCaption(ctx context.Context, chatID int64, messageID int, caption string) error {
	params := &tgbotapi.EditMessageCaptionParams{
		ChatID:    chatID,
		MessageID: messageID,
		Caption:   caption,
	}

	if _, err := t.Bot.EditMessageCaption(ctx, params); err != nil {
		return err
	}

	return nil
}

// EditMessageText edits the text of an existing plain text message.
func (t Telegram) EditMessageText(ctx context.Context, chatID int64, messageID int, text string) error {
	params := &tgbotapi.EditMessageTextParams{
		ChatID:    chatID,
		MessageID: messageID,
		Text:      text,
	}

	if _, err := t.Bot.EditMessageText(ctx, params); err != nil {
		return err
	}

	return nil
}

// DeleteOriginalMessage deletes the original message from the user's chat.
func (t Telegram) DeleteOriginalMessage(ctx context.Context, msg *TelegramMessage) error {
	params := &tgbotapi.DeleteMessageParams{
		ChatID:    msg.Chat.ID,
		MessageID: msg.ID,
	}

	if _, err := t.DeleteMessage(ctx, params); err != nil {
		return err
	}

	return nil
}

// GetFileURL returns the download URL for a given file ID.
func (t Telegram) GetFileURL(ctx context.Context, fileID string) (string, error) {
	file, err := t.GetFile(ctx, &tgbotapi.GetFileParams{FileID: fileID})
	if err != nil {
		return "", err
	}
	return t.FileDownloadLink(file), nil
}

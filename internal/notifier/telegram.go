// Package notifier sends expiry alerts through a Telegram bot.
package notifier

import (
	"fmt"
	"strconv"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// SendMessage sends text to a single chat. chatID may be negative, as is the
// case for Telegram group chats.
func SendMessage(token, chatID, text string) error {
	if token == "" {
		return fmt.Errorf("токен Telegram-бота не настроен")
	}
	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return fmt.Errorf("создание Telegram-бота: %w", err)
	}
	id, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return fmt.Errorf("некорректный chat_id %q: %w", chatID, err)
	}
	msg := tgbotapi.NewMessage(id, text)
	_, err = bot.Send(msg)
	return err
}

// SendToAll sends text to every chat ID, collecting per-chat errors instead
// of aborting on the first failure.
func SendToAll(token string, chatIDs []string, text string) []error {
	var errs []error
	for _, id := range chatIDs {
		if err := SendMessage(token, id, text); err != nil {
			errs = append(errs, fmt.Errorf("chat %s: %w", id, err))
		}
	}
	return errs
}

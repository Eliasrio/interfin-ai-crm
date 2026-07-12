// sender.go — прод-реализация Sender поверх telebot.
//
// Воркер шлёт через bot.Send/Notify напрямую — bot.Start() НЕ нужен и НЕ
// вызывается (баг telebot v3.3.8 с Webhook{Listen:""}, см. internal/telegram).
package worker

import (
	"fmt"

	tele "gopkg.in/telebot.v3"
)

// TelebotSender — Sender поверх живого бота из cmd/server.
type TelebotSender struct {
	bot *tele.Bot
}

func NewTelebotSender(bot *tele.Bot) *TelebotSender {
	return &TelebotSender{bot: bot}
}

// Typing — «печатает…» в чате лида (§6.2 шаг 1, sendChatAction).
func (s *TelebotSender) Typing(chatID int64) error {
	if err := s.bot.Notify(tele.ChatID(chatID), tele.Typing); err != nil {
		return fmt.Errorf("telegram: chat action: %w", err)
	}
	return nil
}

// Send — текст в чат лида (§6.2 шаг 5).
func (s *TelebotSender) Send(chatID int64, text string) error {
	if _, err := s.bot.Send(tele.ChatID(chatID), text); err != nil {
		return fmt.Errorf("telegram: send: %w", err)
	}
	return nil
}

// SendDocument — файл документом (EP-04: PDF из библиотеки файлов).
// fileName — имя, которое увидит клиент (на диске файл лежит под UUID).
func (s *TelebotSender) SendDocument(chatID int64, path, fileName string) error {
	doc := &tele.Document{File: tele.FromDisk(path), FileName: fileName}
	if _, err := s.bot.Send(tele.ChatID(chatID), doc); err != nil {
		return fmt.Errorf("telegram: send document: %w", err)
	}
	return nil
}

// SendPhoto — изображение фотографией (EP-04: JPG/PNG из библиотеки).
func (s *TelebotSender) SendPhoto(chatID int64, path string) error {
	photo := &tele.Photo{File: tele.FromDisk(path)}
	if _, err := s.bot.Send(tele.ChatID(chatID), photo); err != nil {
		return fmt.Errorf("telegram: send photo: %w", err)
	}
	return nil
}

// SendWithKeyboard — текст с постоянной reply-клавиатурой из одной кнопки
// (EP-05, ТЗ §3 вкладка 5): resize + persistent — кнопка остаётся под полем
// ввода у клиента между сообщениями.
func (s *TelebotSender) SendWithKeyboard(chatID int64, text, buttonText string) error {
	markup := &tele.ReplyMarkup{ResizeKeyboard: true, IsPersistent: true}
	markup.Reply(markup.Row(markup.Text(buttonText)))
	if _, err := s.bot.Send(tele.ChatID(chatID), text, markup); err != nil {
		return fmt.Errorf("telegram: send with keyboard: %w", err)
	}
	return nil
}

// SendRemoveKeyboard — текст со снятием reply-клавиатуры (EP-05): кнопку
// выключили в панели — иначе она осталась бы у клиента навсегда. Для чата
// без клавиатуры remove_keyboard — безвредный no-op Telegram.
func (s *TelebotSender) SendRemoveKeyboard(chatID int64, text string) error {
	markup := &tele.ReplyMarkup{RemoveKeyboard: true}
	if _, err := s.bot.Send(tele.ChatID(chatID), text, markup); err != nil {
		return fmt.Errorf("telegram: send remove keyboard: %w", err)
	}
	return nil
}

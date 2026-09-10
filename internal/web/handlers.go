package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/scp-oss/utm-data/internal/models"
	"github.com/scp-oss/utm-data/internal/notifier"
)

type indexData struct {
	Flash *flash
	UTMs  []models.UTM
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	utms, err := s.store.ListUTMs()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, "index", indexData{Flash: flashFromRequest(r), UTMs: utms})
}

type utmFormData struct {
	Flash *flash
	UTM   *models.UTM
}

func (s *Server) handleUTMNewForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, "utm_form", utmFormData{Flash: flashFromRequest(r)})
}

func (s *Server) handleUTMCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	label := strings.TrimSpace(r.FormValue("label"))
	ip := strings.TrimSpace(r.FormValue("ip"))
	port := parsePortOrDefault(r.FormValue("port"))

	if ip == "" {
		redirectWithFlash(w, r, "/utm/new", "error", "Укажите IP-адрес УТМ")
		return
	}

	id, err := s.store.CreateUTM(label, ip, port)
	if err != nil {
		redirectWithFlash(w, r, "/utm/new", "error", "Не удалось сохранить УТМ: "+err.Error())
		return
	}

	u, err := s.store.GetUTM(id)
	if err != nil {
		redirectWithFlash(w, r, "/", "error", "УТМ добавлен, но не найден для опроса: "+err.Error())
		return
	}

	ctx, cancel := pollTimeout(r.Context())
	defer cancel()
	s.sched.PollOne(ctx, *u)

	redirectWithFlash(w, r, "/", "ok", "УТМ добавлен и опрошен")
}

func (s *Server) handleUTMEditForm(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	u, err := s.store.GetUTM(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.render(w, "utm_form", utmFormData{Flash: flashFromRequest(r), UTM: u})
}

func (s *Server) handleUTMUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	label := strings.TrimSpace(r.FormValue("label"))
	port := parsePortOrDefault(r.FormValue("port"))

	if err := s.store.UpdateUTMMeta(id, label, port); err != nil {
		redirectWithFlash(w, r, fmt.Sprintf("/utm/%d/edit", id), "error", "Не удалось сохранить: "+err.Error())
		return
	}
	redirectWithFlash(w, r, "/", "ok", "Изменения сохранены")
}

func (s *Server) handleUTMDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteUTM(id); err != nil {
		redirectWithFlash(w, r, "/", "error", "Не удалось удалить: "+err.Error())
		return
	}
	redirectWithFlash(w, r, "/", "ok", "УТМ удалён")
}

func (s *Server) handleUTMPollNow(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	u, err := s.store.GetUTM(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	ctx, cancel := pollTimeout(r.Context())
	defer cancel()
	s.sched.PollOne(ctx, *u)

	redirectWithFlash(w, r, "/", "ok", "Опрос выполнен")
}

type settingsData struct {
	Flash    *flash
	Settings models.Settings
	Chats    []models.TelegramChat
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := s.store.GetSettings()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	chats, err := s.store.ListTelegramChats()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, "settings", settingsData{Flash: flashFromRequest(r), Settings: settings, Chats: chats})
}

func (s *Server) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	t1 := strings.TrimSpace(r.FormValue("poll_time_1"))
	t2 := strings.TrimSpace(r.FormValue("poll_time_2"))
	token := strings.TrimSpace(r.FormValue("telegram_bot_token"))

	if !validPollTime(t1) {
		redirectWithFlash(w, r, "/settings", "error", "Время опроса 1 указано неверно, используйте формат ЧЧ:ММ")
		return
	}
	if t2 != "" && !validPollTime(t2) {
		redirectWithFlash(w, r, "/settings", "error", "Время опроса 2 указано неверно, используйте формат ЧЧ:ММ")
		return
	}

	err := s.store.UpdateSettings(models.Settings{PollTime1: t1, PollTime2: t2, TelegramBotToken: token})
	if err != nil {
		redirectWithFlash(w, r, "/settings", "error", "Не удалось сохранить настройки: "+err.Error())
		return
	}
	redirectWithFlash(w, r, "/settings", "ok", "Настройки сохранены")
}

func (s *Server) handleTelegramChatAdd(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	chatID := strings.TrimSpace(r.FormValue("chat_id"))
	label := strings.TrimSpace(r.FormValue("chat_label"))

	if _, err := strconv.ParseInt(chatID, 10, 64); err != nil {
		redirectWithFlash(w, r, "/settings", "error", "Chat ID должен быть числом")
		return
	}

	if err := s.store.AddTelegramChat(chatID, label); err != nil {
		redirectWithFlash(w, r, "/settings", "error", "Не удалось добавить получателя: "+err.Error())
		return
	}
	redirectWithFlash(w, r, "/settings", "ok", "Получатель добавлен")
}

func (s *Server) handleTelegramChatDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteTelegramChat(id); err != nil {
		redirectWithFlash(w, r, "/settings", "error", "Не удалось удалить получателя: "+err.Error())
		return
	}
	redirectWithFlash(w, r, "/settings", "ok", "Получатель удалён")
}

func (s *Server) handleTelegramTest(w http.ResponseWriter, r *http.Request) {
	settings, err := s.store.GetSettings()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	chats, err := s.store.ListTelegramChats()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(chats) == 0 {
		redirectWithFlash(w, r, "/settings", "error", "Нет ни одного получателя")
		return
	}

	chatIDs := make([]string, len(chats))
	for i, c := range chats {
		chatIDs[i] = c.ChatID
	}
	errs := notifier.SendToAll(settings.TelegramBotToken, chatIDs, "✅ UTM Дашборд: тестовое сообщение")
	if len(errs) > 0 {
		redirectWithFlash(w, r, "/settings", "error", fmt.Sprintf("Ошибки отправки: %d из %d", len(errs), len(chatIDs)))
		return
	}
	redirectWithFlash(w, r, "/settings", "ok", "Тестовое сообщение отправлено")
}

func idParam(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return 0, false
	}
	return id, true
}

func parsePortOrDefault(s string) int {
	port, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || port <= 0 || port > 65535 {
		return 8080
	}
	return port
}

func validPollTime(s string) bool {
	_, err := time.Parse("15:04", s)
	return err == nil
}

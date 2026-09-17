package web

import (
	"encoding/csv"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/scp-oss/utm-data/internal/models"
	"github.com/scp-oss/utm-data/internal/notifier"
)

type indexData struct {
	base
	UTMs []models.UTM
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	utms, err := s.store.ListUTMs()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, "index", indexData{base: s.pageBase(r), UTMs: utms})
}

type loginData struct {
	base
	Next string
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if s.auth.Authenticated(r) {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	s.render(w, "login", loginData{base: s.pageBase(r), Next: r.URL.Query().Get("next")})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	password := r.FormValue("password")
	next := safeNext(r.FormValue("next"))

	if !s.auth.Check(password) {
		target := "/login?flash_kind=error&flash_text=" + urlEscape("Неверный пароль")
		if next != "/" {
			target += "&next=" + urlEscape(next)
		}
		http.Redirect(w, r, target, http.StatusSeeOther)
		return
	}

	s.auth.Login(w, r)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.auth.Logout(w, r)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// safeNext keeps redirect targets on this site only, refusing to bounce a
// login through an attacker-supplied absolute or protocol-relative URL.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	return next
}

type utmFormData struct {
	base
	UTM *models.UTM
}

func (s *Server) handleUTMNewForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, "utm_form", utmFormData{base: s.pageBase(r)})
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
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			redirectWithFlash(w, r, "/utm/new", "error", fmt.Sprintf("УТМ с адресом %s:%d уже добавлен", ip, port))
			return
		}
		redirectWithFlash(w, r, "/utm/new", "error", "Не удалось сохранить УТМ: "+err.Error())
		return
	}

	u, err := s.store.GetUTM(id)
	if err != nil {
		redirectWithFlash(w, r, "/", "error", "УТМ добавлен, но не найден для опроса: "+err.Error())
		return
	}

	s.sched.PollOneAsync(*u)

	redirectWithFlash(w, r, "/", "ok", "УТМ добавлен, опрос запущен — обновите страницу через 10–30 секунд")
}

// handleUTMBulkAdd takes a pasted list of addresses (one per line, "IP" or
// "IP:порт", port 8080 if omitted) and adds whichever aren't already
// registered — so throwing in a known IP range just adds what's missing
// instead of erroring on every one already present. Every newly added УТМ
// is polled immediately, same as adding one by hand.
func (s *Server) handleUTMBulkAdd(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	existing, err := s.store.ListUTMs()
	if err != nil {
		redirectWithFlash(w, r, "/utm/new", "error", "Не удалось прочитать список УТМ: "+err.Error())
		return
	}
	known := make(map[string]bool, len(existing))
	for _, u := range existing {
		known[fmt.Sprintf("%s:%d", u.IPAddress, u.Port)] = true
	}

	var added, skipped, invalid int
	for _, line := range strings.Split(r.FormValue("ips"), "\n") {
		ip, port, ok := parseIPPortLine(line)
		if !ok {
			if strings.TrimSpace(line) != "" {
				invalid++
			}
			continue
		}
		key := fmt.Sprintf("%s:%d", ip, port)
		if known[key] {
			skipped++
			continue
		}

		id, err := s.store.CreateUTM("", ip, port)
		if err != nil {
			skipped++
			continue
		}
		known[key] = true
		added++
		if u, err := s.store.GetUTM(id); err == nil {
			s.sched.PollOneAsync(*u)
		}
	}

	msg := fmt.Sprintf("Добавлено новых: %d, уже было: %d", added, skipped)
	if invalid > 0 {
		msg += fmt.Sprintf(", не распознано: %d", invalid)
	}
	msg += " — опрос новых запущен"
	redirectWithFlash(w, r, "/", "ok", msg)
}

// parseIPPortLine parses one line of a bulk-add list: a bare IP (port
// defaults to 8080), or "IP:порт". Blank lines and anything that isn't a
// valid IP are rejected via ok=false.
func parseIPPortLine(line string) (ip string, port int, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", 0, false
	}
	ip, port = line, 8080
	if idx := strings.LastIndex(line, ":"); idx != -1 {
		if p, err := strconv.Atoi(line[idx+1:]); err == nil && p > 0 && p <= 65535 {
			ip, port = line[:idx], p
		}
	}
	if net.ParseIP(ip) == nil {
		return "", 0, false
	}
	return ip, port, true
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
	s.render(w, "utm_form", utmFormData{base: s.pageBase(r), UTM: u})
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
	inn := strings.TrimSpace(r.FormValue("inn"))
	kpp := strings.TrimSpace(r.FormValue("kpp"))
	orgName := strings.TrimSpace(r.FormValue("org_name"))
	installAddress := strings.TrimSpace(r.FormValue("install_address"))
	egaisFrom, err1 := parseDateInput(r.FormValue("egais_cert_from"))
	egaisTo, err2 := parseDateInput(r.FormValue("egais_cert_to"))
	gostFrom, err3 := parseDateInput(r.FormValue("gost_cert_from"))
	gostTo, err4 := parseDateInput(r.FormValue("gost_cert_to"))
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		redirectWithFlash(w, r, fmt.Sprintf("/utm/%d/edit", id), "error", "Некорректная дата, используйте формат ГГГГ-ММ-ДД")
		return
	}

	if err := s.store.UpdateUTMMeta(id, label, port, inn, kpp, orgName, installAddress, egaisFrom, egaisTo, gostFrom, gostTo); err != nil {
		redirectWithFlash(w, r, fmt.Sprintf("/utm/%d/edit", id), "error", "Не удалось сохранить: "+err.Error())
		return
	}
	redirectWithFlash(w, r, "/", "ok", "Изменения сохранены")
}

// parseDateInput parses an HTML <input type=date> value ("" means "clear").
func parseDateInput(s string) (*time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return nil, err
	}
	return &t, nil
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

	s.sched.PollOneAsync(*u)

	redirectWithFlash(w, r, "/", "ok", "Опрос запущен — обновите страницу через 10–30 секунд")
}

type settingsData struct {
	base
	Settings models.Settings
	Chats    []models.TelegramChat
	UTMs     []models.UTM
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
	utms, err := s.store.ListUTMs()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, "settings", settingsData{base: s.pageBase(r), Settings: settings, Chats: chats, UTMs: utms})
}

func (s *Server) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	t1 := strings.TrimSpace(r.FormValue("poll_time_1"))
	t2 := strings.TrimSpace(r.FormValue("poll_time_2"))
	token := strings.TrimSpace(r.FormValue("telegram_bot_token"))
	mode := models.TelegramMode(r.FormValue("telegram_mode"))
	proxyURL := strings.TrimSpace(r.FormValue("telegram_proxy_url"))
	relayBase := strings.TrimSpace(r.FormValue("telegram_relay_base_url"))
	relayAuth := strings.TrimSpace(r.FormValue("telegram_relay_auth_key"))

	if !validPollTime(t1) {
		redirectWithFlash(w, r, "/settings", "error", "Время опроса 1 указано неверно, используйте формат ЧЧ:ММ")
		return
	}
	if t2 != "" && !validPollTime(t2) {
		redirectWithFlash(w, r, "/settings", "error", "Время опроса 2 указано неверно, используйте формат ЧЧ:ММ")
		return
	}
	switch mode {
	case models.TelegramModeDirect, models.TelegramModeSocks5, models.TelegramModeRelay:
	default:
		mode = models.TelegramModeDirect
	}

	err := s.store.UpdateSettings(models.Settings{
		PollTime1:            t1,
		PollTime2:            t2,
		TelegramBotToken:     token,
		TelegramMode:         mode,
		TelegramProxyURL:     proxyURL,
		TelegramRelayBaseURL: relayBase,
		TelegramRelayAuthKey: relayAuth,
	})
	if err != nil {
		redirectWithFlash(w, r, "/settings", "error", "Не удалось сохранить настройки: "+err.Error())
		return
	}
	redirectWithFlash(w, r, "/settings", "ok", "Настройки сохранены")
}

// handleUTMExport downloads every УТМ's IP/port/label as CSV — a simple
// backup so the list can be restored (or copied to another instance)
// without needing the database file itself. Organization info and
// certificate dates are deliberately left out: they get filled back in by
// the next poll, and re-typing them on import would just risk going stale.
func (s *Server) handleUTMExport(w http.ResponseWriter, r *http.Request) {
	utms, err := s.store.ListUTMs()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="utm-export.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"ip_address", "port", "label"})
	for _, u := range utms {
		port := ""
		if u.Port != 8080 {
			port = strconv.Itoa(u.Port)
		}
		_ = cw.Write([]string{u.IPAddress, port, u.Label})
	}
	cw.Flush()
}

// handleUTMImport bulk-adds УТМ from a CSV file in the format handleUTMExport
// produces (ip_address,port,label — port/label optional, header row
// optional). Rows with an invalid IP, or an ip:port pair already in the
// database, are skipped rather than failing the whole import. Every newly
// added УТМ is polled immediately, same as adding one by hand.
func (s *Server) handleUTMImport(w http.ResponseWriter, r *http.Request) {
	file, _, err := r.FormFile("file")
	if err != nil {
		redirectWithFlash(w, r, "/settings", "error", "Выберите CSV-файл для импорта")
		return
	}
	defer file.Close()

	existing, err := s.store.ListUTMs()
	if err != nil {
		redirectWithFlash(w, r, "/settings", "error", "Импорт не удался: "+err.Error())
		return
	}
	known := make(map[string]bool, len(existing))
	for _, u := range existing {
		known[fmt.Sprintf("%s:%d", u.IPAddress, u.Port)] = true
	}

	cr := csv.NewReader(file)
	cr.FieldsPerRecord = -1
	rows, err := cr.ReadAll()
	if err != nil {
		redirectWithFlash(w, r, "/settings", "error", "Не удалось прочитать CSV: "+err.Error())
		return
	}

	var added, skipped int
	for _, row := range rows {
		if len(row) == 0 {
			continue
		}
		ip := strings.TrimSpace(row[0])
		if ip == "" || strings.EqualFold(ip, "ip_address") || strings.EqualFold(ip, "ip") {
			continue // blank line or header row
		}
		if net.ParseIP(ip) == nil {
			skipped++
			continue
		}
		port := 8080
		if len(row) > 1 {
			port = parsePortOrDefault(row[1])
		}
		key := fmt.Sprintf("%s:%d", ip, port)
		if known[key] {
			skipped++
			continue
		}
		label := ""
		if len(row) > 2 {
			label = strings.TrimSpace(row[2])
		}

		id, err := s.store.CreateUTM(label, ip, port)
		if err != nil {
			skipped++
			continue
		}
		known[key] = true
		added++
		if u, err := s.store.GetUTM(id); err == nil {
			s.sched.PollOneAsync(*u)
		}
	}

	redirectWithFlash(w, r, "/settings", "ok", fmt.Sprintf("Импорт готов: добавлено %d, пропущено %d — опрос запущен", added, skipped))
}

func (s *Server) handleTelegramChatAdd(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	chatID := strings.TrimSpace(r.FormValue("chat_id"))
	label := strings.TrimSpace(r.FormValue("chat_label"))
	utmIDStr := strings.TrimSpace(r.FormValue("utm_id"))

	if _, err := strconv.ParseInt(chatID, 10, 64); err != nil {
		redirectWithFlash(w, r, "/settings", "error", "Chat ID должен быть числом")
		return
	}

	var utmID *int64
	if utmIDStr != "" {
		id, err := strconv.ParseInt(utmIDStr, 10, 64)
		if err != nil {
			redirectWithFlash(w, r, "/settings", "error", "Некорректный УТМ")
			return
		}
		utmID = &id
	}

	if err := s.store.AddTelegramChat(chatID, label, utmID); err != nil {
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
	errs := notifier.SendToAll(settings, chatIDs, fmt.Sprintf("✅ УТМ: сроки сертификатов — тестовое сообщение (режим: %s)", settings.TelegramMode))
	if len(errs) > 0 {
		redirectWithFlash(w, r, "/settings", "error", fmt.Sprintf("Ошибки отправки: %d из %d (%v)", len(errs), len(chatIDs), errs[0]))
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

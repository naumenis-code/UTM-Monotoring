// Package notifier sends expiry alerts through a Telegram bot.
//
// api.telegram.org is blocked on some Russian networks, so every send reads
// the delivery mode fresh from Settings and routes accordingly:
//
//   - direct: a plain HTTPS call to api.telegram.org.
//   - socks5: the same call tunneled through a SOCKS5 proxy.
//   - relay:  a third-party HTTP relay that mirrors the Bot API under its
//     own domain (commonly used to route around the block) and expects an
//     extra "auth" query parameter of its own, unrelated to the bot token.
//
// Because the mode/proxy/relay settings are read from the database on every
// call rather than cached, changing them in the "Настройки" page takes
// effect on the very next notification with no restart needed.
package notifier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/proxy"

	"github.com/scp-oss/utm-data/internal/models"
)

const directAPIBase = "https://api.telegram.org"

// Send delivers text to a single chat according to the given settings.
// chatID may be negative, as is the case for Telegram group chats.
func Send(settings models.Settings, chatID, text string) error {
	if settings.TelegramBotToken == "" {
		return fmt.Errorf("токен Telegram-бота не настроен")
	}

	endpoint, client, err := buildRequest(settings)
	if err != nil {
		return err
	}

	payload, err := json.Marshal(map[string]string{"chat_id": chatID, "text": text})
	if err != nil {
		return fmt.Errorf("encode payload: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("send telegram message (%s): %w", settings.TelegramMode, err)
	}
	defer resp.Body.Close()

	var body struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)

	if resp.StatusCode != http.StatusOK || !body.OK {
		if body.Description != "" {
			return fmt.Errorf("telegram API: %s", body.Description)
		}
		return fmt.Errorf("telegram API вернул статус %d", resp.StatusCode)
	}
	return nil
}

// SendToAll sends text to every chat, collecting per-chat errors instead of
// aborting on the first failure.
func SendToAll(settings models.Settings, chatIDs []string, text string) []error {
	var errs []error
	for _, id := range chatIDs {
		if err := Send(settings, id, text); err != nil {
			errs = append(errs, fmt.Errorf("chat %s: %w", id, err))
		}
	}
	return errs
}

func buildRequest(settings models.Settings) (endpoint string, client *http.Client, err error) {
	method := "sendMessage"
	client = &http.Client{Timeout: 15 * time.Second}

	switch settings.TelegramMode {
	case models.TelegramModeSocks5:
		if settings.TelegramProxyURL == "" {
			return "", nil, fmt.Errorf("не задан адрес SOCKS5-прокси")
		}
		dialer, err := socks5Dialer(settings.TelegramProxyURL)
		if err != nil {
			return "", nil, err
		}
		client.Transport = &http.Transport{
			DialContext: func(_ context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			},
		}
		endpoint = fmt.Sprintf("%s/bot%s/%s", directAPIBase, settings.TelegramBotToken, method)

	case models.TelegramModeRelay:
		base := strings.TrimRight(settings.TelegramRelayBaseURL, "/")
		if base == "" {
			return "", nil, fmt.Errorf("не задан адрес relay-сервера")
		}
		endpoint = fmt.Sprintf("%s/bot%s/%s", base, settings.TelegramBotToken, method)
		if settings.TelegramRelayAuthKey != "" {
			endpoint += "?auth=" + url.QueryEscape(settings.TelegramRelayAuthKey)
		}

	default: // TelegramModeDirect and unset/legacy values
		endpoint = fmt.Sprintf("%s/bot%s/%s", directAPIBase, settings.TelegramBotToken, method)
	}

	return endpoint, client, nil
}

func socks5Dialer(proxyURL string) (proxy.Dialer, error) {
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("некорректный адрес SOCKS5-прокси: %w", err)
	}
	var auth *proxy.Auth
	if u.User != nil {
		auth = &proxy.Auth{User: u.User.Username()}
		if pw, ok := u.User.Password(); ok {
			auth.Password = pw
		}
	}
	dialer, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("создание SOCKS5-клиента: %w", err)
	}
	return dialer, nil
}

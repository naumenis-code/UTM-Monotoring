// Package models defines the domain types shared across the application.
package models

import "time"

// UTM represents a single monitored УТМ (Универсальный транспортный модуль) instance.
type UTM struct {
	ID              int64
	Label           string
	IPAddress       string
	Port            int
	FSRARID         string
	INN             string
	KPP             string
	OrgName         string
	InstallAddress  string
	EgaisCertFrom   *time.Time
	EgaisCertTo     *time.Time
	GostCertFrom    *time.Time
	GostCertTo      *time.Time
	LastPolledAt    *time.Time
	LastPollOK      bool
	LastPollError   string
	LastRawResponse string
	// ConsecutivePollFailures and FailingSince track an unbroken run of
	// failed polls, reset to 0/nil the moment one succeeds — so the
	// dashboard can show "не отвечает N дней" to help decide whether a УТМ
	// is just temporarily down or should be removed (decommissioned).
	ConsecutivePollFailures int
	FailingSince            *time.Time
	CreatedAt               time.Time
}

// TelegramMode selects how outgoing Telegram API calls are routed. All three
// exist because api.telegram.org is blocked on some Russian networks: a
// direct connection fails there, so the bot also needs to work through a
// SOCKS5 proxy or through a third-party HTTP relay that mirrors the Bot API
// under a different domain (and typically requires its own "auth" key).
type TelegramMode string

const (
	TelegramModeDirect TelegramMode = "direct"
	TelegramModeSocks5 TelegramMode = "socks5"
	TelegramModeRelay  TelegramMode = "relay"
)

// Settings holds the single global configuration row.
type Settings struct {
	PollTime1        string // "HH:MM", empty disables this slot
	PollTime2        string // "HH:MM", empty disables this slot
	TelegramBotToken string

	TelegramMode         TelegramMode
	TelegramProxyURL     string // socks5://[user:pass@]host:port, mode=socks5
	TelegramRelayBaseURL string // e.g. https://ваш-relay, mode=relay
	TelegramRelayAuthKey string // relay's own "auth" query parameter, mode=relay
}

// TelegramChat is a chat/group that receives expiry notifications. UTMID
// scopes the recipient to a single client's УТМ (so different customers'
// contacts don't see each other's alerts); nil means "all УТМ" and is meant
// for an internal/admin recipient.
type TelegramChat struct {
	ID       int64
	ChatID   string
	Label    string
	UTMID    *int64
	UTMLabel string // joined for display only, empty when UTMID is nil
}

// CertType identifies which certificate a notification/threshold refers to.
type CertType string

const (
	CertEgais CertType = "egais"
	CertGost  CertType = "gost"
)

// NotificationThresholds are the "days before expiry" checkpoints that trigger
// a Telegram alert, ordered from farthest to closest to expiry.
var NotificationThresholds = []int{30, 10, 5, 2, 1}

// ShortLabel is used in the compact Telegram digest, where several items
// share one message and a long label would push lines to wrap awkwardly on
// a phone screen.
func (c CertType) ShortLabel() string {
	switch c {
	case CertEgais:
		return "ЕГАИС"
	case CertGost:
		return "ГОСТ"
	default:
		return string(c)
	}
}

func (c CertType) Label() string {
	switch c {
	case CertEgais:
		return "сертификат доступа к ЕГАИС"
	case CertGost:
		return "ГОСТ-сертификат"
	default:
		return string(c)
	}
}

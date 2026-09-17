// Package store implements the data-access layer on top of SQLite.
package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/scp-oss/utm-data/internal/models"
)

type Store struct {
	db *sql.DB
}

func New(db *sql.DB) *Store {
	return &Store{db: db}
}

const timeLayout = time.RFC3339

func formatTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(timeLayout)
}

func parseTime(s sql.NullString) *time.Time {
	if !s.Valid || s.String == "" {
		return nil
	}
	t, err := time.Parse(timeLayout, s.String)
	if err != nil {
		return nil
	}
	return &t
}

// ---- UTMs ----

func (s *Store) CreateUTM(label, ip string, port int) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO utms (label, ip_address, port, created_at) VALUES (?, ?, ?, ?)`,
		label, ip, port, time.Now().UTC().Format(timeLayout),
	)
	if err != nil {
		return 0, fmt.Errorf("insert utm: %w", err)
	}
	return res.LastInsertId()
}

func (s *Store) DeleteUTM(id int64) error {
	_, err := s.db.Exec(`DELETE FROM utms WHERE id = ?`, id)
	return err
}

// UpdateUTMMeta saves the operator-editable fields: the label/port chosen
// when the УТМ was added, organization info, and certificate validity
// dates. All of these are editable by hand because the automatic lookup
// (an undocumented local УТМ API, plus a /home date scrape fallback — see
// internal/utmclient) is a best-effort convenience, not a guaranteed
// contract on every device/version — mirroring how 1С itself treats
// "Запросить из ЕГАИС" as one option alongside manual entry of the same
// fields. Automatic polling still overwrites whatever it manages to
// determine itself; see scheduler.PollOne.
func (s *Store) UpdateUTMMeta(id int64, label string, port int, inn, kpp, orgName, installAddress string, egaisFrom, egaisTo, gostFrom, gostTo *time.Time) error {
	_, err := s.db.Exec(`UPDATE utms SET label = ?, port = ?,
		inn = ?, kpp = ?, org_name = ?, install_address = ?,
		egais_cert_from = ?, egais_cert_to = ?, gost_cert_from = ?, gost_cert_to = ?
		WHERE id = ?`,
		label, port, inn, kpp, orgName, installAddress,
		formatTime(egaisFrom), formatTime(egaisTo), formatTime(gostFrom), formatTime(gostTo), id)
	return err
}

func (s *Store) GetUTM(id int64) (*models.UTM, error) {
	row := s.db.QueryRow(utmSelect+` WHERE id = ?`, id)
	return scanUTM(row)
}

// ListUTMs orders by whichever certificate (ЕГАИС or ГОСТ) expires
// soonest, ascending — the УТМ needing attention first is at the top, and
// one with more time left (or no cert data at all yet) sinks toward the
// bottom. Dates are stored as RFC3339 UTC text, which sorts correctly as a
// plain string; a UTM missing both dates sorts as if it expired in 9999,
// i.e. last. min(x, y) here is SQLite's scalar two-argument form, not the
// single-argument aggregate.
func (s *Store) ListUTMs() ([]models.UTM, error) {
	rows, err := s.db.Query(utmSelect + `
		ORDER BY min(COALESCE(egais_cert_to, '9999-12-31T23:59:59Z'), COALESCE(gost_cert_to, '9999-12-31T23:59:59Z')) ASC,
		         id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list utms: %w", err)
	}
	defer rows.Close()

	var out []models.UTM
	for rows.Next() {
		u, err := scanUTM(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

const utmSelect = `SELECT id, label, ip_address, port, fsrar_id, inn, kpp, org_name, install_address,
	egais_cert_from, egais_cert_to, gost_cert_from, gost_cert_to,
	last_polled_at, last_poll_ok, last_poll_error, last_raw_response,
	consecutive_poll_failures, failing_since, created_at
	FROM utms`

type scanner interface {
	Scan(dest ...any) error
}

func scanUTM(row scanner) (*models.UTM, error) {
	var u models.UTM
	var egaisFrom, egaisTo, gostFrom, gostTo, lastPolledAt, failingSince sql.NullString
	var lastPollOK int
	var createdAt string
	err := row.Scan(
		&u.ID, &u.Label, &u.IPAddress, &u.Port, &u.FSRARID, &u.INN, &u.KPP, &u.OrgName, &u.InstallAddress,
		&egaisFrom, &egaisTo, &gostFrom, &gostTo,
		&lastPolledAt, &lastPollOK, &u.LastPollError, &u.LastRawResponse,
		&u.ConsecutivePollFailures, &failingSince, &createdAt,
	)
	if err != nil {
		return nil, err
	}
	u.EgaisCertFrom = parseTime(egaisFrom)
	u.EgaisCertTo = parseTime(egaisTo)
	u.GostCertFrom = parseTime(gostFrom)
	u.GostCertTo = parseTime(gostTo)
	u.LastPolledAt = parseTime(lastPolledAt)
	u.LastPollOK = lastPollOK != 0
	u.FailingSince = parseTime(failingSince)
	if t, err := time.Parse(timeLayout, createdAt); err == nil {
		u.CreatedAt = t
	}
	return &u, nil
}

// PollResult carries the outcome of polling a УТМ's HTTP API.
type PollResult struct {
	FSRARID        string
	INN            string
	KPP            string
	OrgName        string
	InstallAddress string
	EgaisCertFrom  *time.Time
	EgaisCertTo    *time.Time
	GostCertFrom   *time.Time
	GostCertTo     *time.Time
	OK             bool
	Error          string
	RawResponse    string
}

// SaveUTMPollResult persists a poll's outcome and updates the
// consecutive-failure streak used to show "не отвечает N дней": a success
// resets it to zero and clears failing_since, a failure increments it and
// sets failing_since only if it wasn't already tracking a streak (so the
// streak's start date doesn't move on every subsequent failure).
func (s *Store) SaveUTMPollResult(id int64, r PollResult) error {
	now := time.Now().UTC().Format(timeLayout)
	ok := boolToInt(r.OK)
	_, err := s.db.Exec(`UPDATE utms SET
		fsrar_id = ?, inn = ?, kpp = ?, org_name = ?, install_address = ?,
		egais_cert_from = ?, egais_cert_to = ?, gost_cert_from = ?, gost_cert_to = ?,
		last_polled_at = ?, last_poll_ok = ?, last_poll_error = ?, last_raw_response = ?,
		consecutive_poll_failures = CASE WHEN ? THEN 0 ELSE consecutive_poll_failures + 1 END,
		failing_since = CASE WHEN ? THEN NULL WHEN failing_since IS NULL THEN ? ELSE failing_since END
		WHERE id = ?`,
		r.FSRARID, r.INN, r.KPP, r.OrgName, r.InstallAddress,
		formatTime(r.EgaisCertFrom), formatTime(r.EgaisCertTo), formatTime(r.GostCertFrom), formatTime(r.GostCertTo),
		now, ok, r.Error, r.RawResponse,
		ok, ok, now,
		id,
	)
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---- Settings ----

func (s *Store) GetSettings() (models.Settings, error) {
	var st models.Settings
	var mode string
	err := s.db.QueryRow(`SELECT poll_time_1, poll_time_2, telegram_bot_token,
		telegram_mode, telegram_proxy_url, telegram_relay_base_url, telegram_relay_auth_key
		FROM settings WHERE id = 1`).
		Scan(&st.PollTime1, &st.PollTime2, &st.TelegramBotToken,
			&mode, &st.TelegramProxyURL, &st.TelegramRelayBaseURL, &st.TelegramRelayAuthKey)
	st.TelegramMode = models.TelegramMode(mode)
	if st.TelegramMode == "" {
		st.TelegramMode = models.TelegramModeDirect
	}
	return st, err
}

func (s *Store) UpdateSettings(st models.Settings) error {
	_, err := s.db.Exec(`UPDATE settings SET poll_time_1 = ?, poll_time_2 = ?, telegram_bot_token = ?,
		telegram_mode = ?, telegram_proxy_url = ?, telegram_relay_base_url = ?, telegram_relay_auth_key = ?
		WHERE id = 1`,
		st.PollTime1, st.PollTime2, st.TelegramBotToken,
		string(st.TelegramMode), st.TelegramProxyURL, st.TelegramRelayBaseURL, st.TelegramRelayAuthKey)
	return err
}

// ---- Telegram chats ----

const telegramChatSelect = `SELECT tc.id, tc.chat_id, tc.label, tc.utm_id, COALESCE(u.label, u.org_name, u.ip_address, '')
	FROM telegram_chats tc LEFT JOIN utms u ON u.id = tc.utm_id`

func (s *Store) ListTelegramChats() ([]models.TelegramChat, error) {
	rows, err := s.db.Query(telegramChatSelect + ` ORDER BY tc.id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTelegramChats(rows)
}

// ListTelegramChatsForUTM returns the recipients that should be notified
// about a given УТМ: those scoped to it plus the ones scoped to no УТМ at
// all (utm_id IS NULL), which act as "receives everything" admins.
func (s *Store) ListTelegramChatsForUTM(utmID int64) ([]models.TelegramChat, error) {
	rows, err := s.db.Query(telegramChatSelect+` WHERE tc.utm_id IS NULL OR tc.utm_id = ? ORDER BY tc.id ASC`, utmID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTelegramChats(rows)
}

func scanTelegramChats(rows *sql.Rows) ([]models.TelegramChat, error) {
	var out []models.TelegramChat
	for rows.Next() {
		var c models.TelegramChat
		var utmID sql.NullInt64
		if err := rows.Scan(&c.ID, &c.ChatID, &c.Label, &utmID, &c.UTMLabel); err != nil {
			return nil, err
		}
		if utmID.Valid {
			c.UTMID = &utmID.Int64
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) AddTelegramChat(chatID, label string, utmID *int64) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO telegram_chats (chat_id, label, utm_id) VALUES (?, ?, ?)`, chatID, label, utmID)
	return err
}

func (s *Store) DeleteTelegramChat(id int64) error {
	_, err := s.db.Exec(`DELETE FROM telegram_chats WHERE id = ?`, id)
	return err
}

// ---- Notifications ----

// NotificationAlreadySent reports whether an alert for this exact
// (utm, cert type, expiry date, threshold) combination was already sent.
// The expiry date is part of the key so that renewing a certificate
// (which changes its expiry date) naturally resets the alert schedule.
func (s *Store) NotificationAlreadySent(utmID int64, certType models.CertType, expiry time.Time, thresholdDays int) (bool, error) {
	var exists int
	err := s.db.QueryRow(
		`SELECT 1 FROM notifications_sent WHERE utm_id = ? AND cert_type = ? AND expiry_date = ? AND threshold_days = ?`,
		utmID, string(certType), expiry.UTC().Format("2006-01-02"), thresholdDays,
	).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) MarkNotificationSent(utmID int64, certType models.CertType, expiry time.Time, thresholdDays int) error {
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO notifications_sent (utm_id, cert_type, expiry_date, threshold_days, sent_at) VALUES (?, ?, ?, ?, ?)`,
		utmID, string(certType), expiry.UTC().Format("2006-01-02"), thresholdDays, time.Now().UTC().Format(timeLayout),
	)
	return err
}

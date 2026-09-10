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

func (s *Store) UpdateUTMMeta(id int64, label string, port int) error {
	_, err := s.db.Exec(`UPDATE utms SET label = ?, port = ? WHERE id = ?`, label, port, id)
	return err
}

func (s *Store) GetUTM(id int64) (*models.UTM, error) {
	row := s.db.QueryRow(utmSelect+` WHERE id = ?`, id)
	return scanUTM(row)
}

func (s *Store) ListUTMs() ([]models.UTM, error) {
	rows, err := s.db.Query(utmSelect + ` ORDER BY created_at ASC`)
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

const utmSelect = `SELECT id, label, ip_address, port, inn, kpp, org_name, install_address,
	egais_cert_from, egais_cert_to, gost_cert_from, gost_cert_to,
	last_polled_at, last_poll_ok, last_poll_error, last_raw_response, created_at
	FROM utms`

type scanner interface {
	Scan(dest ...any) error
}

func scanUTM(row scanner) (*models.UTM, error) {
	var u models.UTM
	var egaisFrom, egaisTo, gostFrom, gostTo, lastPolledAt sql.NullString
	var lastPollOK int
	var createdAt string
	err := row.Scan(
		&u.ID, &u.Label, &u.IPAddress, &u.Port, &u.INN, &u.KPP, &u.OrgName, &u.InstallAddress,
		&egaisFrom, &egaisTo, &gostFrom, &gostTo,
		&lastPolledAt, &lastPollOK, &u.LastPollError, &u.LastRawResponse, &createdAt,
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
	if t, err := time.Parse(timeLayout, createdAt); err == nil {
		u.CreatedAt = t
	}
	return &u, nil
}

// PollResult carries the outcome of polling a УТМ's HTTP API.
type PollResult struct {
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

func (s *Store) SaveUTMPollResult(id int64, r PollResult) error {
	_, err := s.db.Exec(`UPDATE utms SET
		inn = ?, kpp = ?, org_name = ?, install_address = ?,
		egais_cert_from = ?, egais_cert_to = ?, gost_cert_from = ?, gost_cert_to = ?,
		last_polled_at = ?, last_poll_ok = ?, last_poll_error = ?, last_raw_response = ?
		WHERE id = ?`,
		r.INN, r.KPP, r.OrgName, r.InstallAddress,
		formatTime(r.EgaisCertFrom), formatTime(r.EgaisCertTo), formatTime(r.GostCertFrom), formatTime(r.GostCertTo),
		time.Now().UTC().Format(timeLayout), boolToInt(r.OK), r.Error, r.RawResponse,
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
	err := s.db.QueryRow(`SELECT poll_time_1, poll_time_2, telegram_bot_token FROM settings WHERE id = 1`).
		Scan(&st.PollTime1, &st.PollTime2, &st.TelegramBotToken)
	return st, err
}

func (s *Store) UpdateSettings(st models.Settings) error {
	_, err := s.db.Exec(`UPDATE settings SET poll_time_1 = ?, poll_time_2 = ?, telegram_bot_token = ? WHERE id = 1`,
		st.PollTime1, st.PollTime2, st.TelegramBotToken)
	return err
}

// ---- Telegram chats ----

func (s *Store) ListTelegramChats() ([]models.TelegramChat, error) {
	rows, err := s.db.Query(`SELECT id, chat_id, label FROM telegram_chats ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.TelegramChat
	for rows.Next() {
		var c models.TelegramChat
		if err := rows.Scan(&c.ID, &c.ChatID, &c.Label); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) AddTelegramChat(chatID, label string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO telegram_chats (chat_id, label) VALUES (?, ?)`, chatID, label)
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

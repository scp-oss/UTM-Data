// Package models defines the domain types shared across the application.
package models

import "time"

// UTM represents a single monitored УТМ (Универсальный транспортный модуль) instance.
type UTM struct {
	ID              int64
	Label           string
	IPAddress       string
	Port            int
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
	CreatedAt       time.Time
}

// Settings holds the single global configuration row.
type Settings struct {
	PollTime1        string // "HH:MM", empty disables this slot
	PollTime2        string // "HH:MM", empty disables this slot
	TelegramBotToken string
}

// TelegramChat is a chat/group that receives expiry notifications.
type TelegramChat struct {
	ID     int64
	ChatID string
	Label  string
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

CREATE TABLE IF NOT EXISTS utms (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    label              TEXT NOT NULL DEFAULT '',
    ip_address         TEXT NOT NULL,
    port               INTEGER NOT NULL DEFAULT 8080,
    inn                TEXT NOT NULL DEFAULT '',
    kpp                TEXT NOT NULL DEFAULT '',
    org_name           TEXT NOT NULL DEFAULT '',
    install_address    TEXT NOT NULL DEFAULT '',
    egais_cert_from    TEXT,
    egais_cert_to      TEXT,
    gost_cert_from     TEXT,
    gost_cert_to       TEXT,
    last_polled_at     TEXT,
    last_poll_ok       INTEGER NOT NULL DEFAULT 0,
    last_poll_error    TEXT NOT NULL DEFAULT '',
    last_raw_response  TEXT NOT NULL DEFAULT '',
    created_at         TEXT NOT NULL,
    UNIQUE (ip_address, port)
);

CREATE TABLE IF NOT EXISTS settings (
    id                   INTEGER PRIMARY KEY CHECK (id = 1),
    poll_time_1          TEXT NOT NULL DEFAULT '09:00',
    poll_time_2          TEXT NOT NULL DEFAULT '',
    telegram_bot_token   TEXT NOT NULL DEFAULT ''
);

INSERT OR IGNORE INTO settings (id, poll_time_1, poll_time_2, telegram_bot_token)
VALUES (1, '09:00', '', '');

CREATE TABLE IF NOT EXISTS telegram_chats (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    chat_id  TEXT NOT NULL UNIQUE,
    label    TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS notifications_sent (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    utm_id          INTEGER NOT NULL REFERENCES utms(id) ON DELETE CASCADE,
    cert_type       TEXT NOT NULL,
    expiry_date     TEXT NOT NULL,
    threshold_days  INTEGER NOT NULL,
    sent_at         TEXT NOT NULL,
    UNIQUE (utm_id, cert_type, expiry_date, threshold_days)
);

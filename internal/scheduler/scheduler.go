// Package scheduler drives periodic УТМ polling and expiry notifications.
package scheduler

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/scp-oss/utm-data/internal/models"
	"github.com/scp-oss/utm-data/internal/notifier"
	"github.com/scp-oss/utm-data/internal/store"
	"github.com/scp-oss/utm-data/internal/utmclient"
)

type Scheduler struct {
	store  *store.Store
	client *utmclient.Client

	mu          sync.Mutex
	lastRunDate map[string]string // slot name -> "2006-01-02" already triggered
}

// backgroundPollTimeout bounds a single async poll kicked off from the web
// UI (see PollOneAsync). It must comfortably exceed utmclient's own
// replyWaitTimeout (120s) so a slow but successful ЕГАИС round trip isn't
// cut short.
const backgroundPollTimeout = 150 * time.Second

func New(st *store.Store, client *utmclient.Client) *Scheduler {
	return &Scheduler{
		store:       st,
		client:      client,
		lastRunDate: make(map[string]string),
	}
}

// Run blocks, checking every minute whether one of the two configured daily
// poll times has just been reached, until ctx is canceled.
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.tick(ctx, now)
		}
	}
}

func (s *Scheduler) tick(ctx context.Context, now time.Time) {
	settings, err := s.store.GetSettings()
	if err != nil {
		log.Printf("scheduler: read settings: %v", err)
		return
	}

	hhmm := now.Format("15:04")
	today := now.Format("2006-01-02")

	slots := []struct{ name, value string }{
		{"1", settings.PollTime1},
		{"2", settings.PollTime2},
	}

	for _, slot := range slots {
		if slot.value == "" || slot.value != hhmm {
			continue
		}

		s.mu.Lock()
		already := s.lastRunDate[slot.name] == today
		s.lastRunDate[slot.name] = today
		s.mu.Unlock()

		if already {
			continue
		}

		go s.RunCycle(ctx)
	}
}

// RunCycle polls every registered УТМ and then checks/sends expiry
// notifications. It is used by the scheduled ticks, by the manual
// "poll now" action and immediately after a new УТМ is added.
func (s *Scheduler) RunCycle(ctx context.Context) {
	utms, err := s.store.ListUTMs()
	if err != nil {
		log.Printf("scheduler: list utms: %v", err)
		return
	}
	for _, u := range utms {
		s.PollOne(ctx, u)
	}
	s.CheckAndNotify(ctx)
}

// PollOneAsync starts PollOne in the background using a context detached
// from the caller (in particular, from any inbound HTTP request). A single
// poll can take up to ~45s — it waits on an async round trip through the
// УТМ to the central ЕГАИС server — so tying it to a request's own context
// would let a page refresh, a closed tab, or a proxy's idle timeout cancel
// an otherwise-successful poll partway through (surfacing as a confusing
// "context canceled" error). Progress is visible on the next page load via
// the УТМ's last_poll_* fields; this call does not wait for it to finish.
func (s *Scheduler) PollOneAsync(u models.UTM) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), backgroundPollTimeout)
		defer cancel()
		s.PollOne(ctx, u)
	}()
}

// PollOne fetches fresh data for a single УТМ and persists the result. A
// partial or failed poll never erases previously known good data (org
// info fetched earlier, or certificate dates entered by hand): only fields
// this poll actually determined are overwritten.
func (s *Scheduler) PollOne(ctx context.Context, u models.UTM) {
	info, err := s.client.FetchInfo(ctx, u.IPAddress, u.Port)

	result := store.PollResult{
		OK:             err == nil,
		FSRARID:        u.FSRARID,
		INN:            u.INN,
		KPP:            u.KPP,
		OrgName:        u.OrgName,
		InstallAddress: u.InstallAddress,
		EgaisCertFrom:  u.EgaisCertFrom,
		EgaisCertTo:    u.EgaisCertTo,
		GostCertFrom:   u.GostCertFrom,
		GostCertTo:     u.GostCertTo,
	}
	if err != nil {
		result.Error = err.Error()
		log.Printf("poll утм #%d (%s:%d): %v", u.ID, u.IPAddress, u.Port, err)
	}
	if info != nil {
		result.RawResponse = info.RawResponse
		if info.FSRARID != "" {
			result.FSRARID = info.FSRARID
		}
		if info.INN != "" {
			result.INN = info.INN
		}
		if info.KPP != "" {
			result.KPP = info.KPP
		}
		if info.OrgName != "" {
			result.OrgName = info.OrgName
		}
		if info.InstallAddress != "" {
			result.InstallAddress = info.InstallAddress
		}
		if info.EgaisCertFrom != nil {
			result.EgaisCertFrom = info.EgaisCertFrom
		}
		if info.EgaisCertTo != nil {
			result.EgaisCertTo = info.EgaisCertTo
		}
		if info.GostCertFrom != nil {
			result.GostCertFrom = info.GostCertFrom
		}
		if info.GostCertTo != nil {
			result.GostCertTo = info.GostCertTo
		}
	}

	if saveErr := s.store.SaveUTMPollResult(u.ID, result); saveErr != nil {
		log.Printf("poll утм #%d: save result: %v", u.ID, saveErr)
	}
}

// CheckAndNotify scans all УТМ certificates and sends Telegram alerts for
// any (certificate, threshold) combination that has become due and was not
// already sent for the certificate's current expiry date. Each УТМ notifies
// only its own scoped recipients plus the unscoped ("all УТМ") ones, so
// different clients' contacts never see each other's alerts.
func (s *Scheduler) CheckAndNotify(ctx context.Context) {
	settings, err := s.store.GetSettings()
	if err != nil {
		log.Printf("notify: read settings: %v", err)
		return
	}
	if settings.TelegramBotToken == "" {
		return
	}

	utms, err := s.store.ListUTMs()
	if err != nil {
		log.Printf("notify: list utms: %v", err)
		return
	}

	now := time.Now()
	for _, u := range utms {
		chats, err := s.store.ListTelegramChatsForUTM(u.ID)
		if err != nil {
			log.Printf("notify: list chats for утм #%d: %v", u.ID, err)
			continue
		}
		if len(chats) == 0 {
			continue
		}
		chatIDs := make([]string, len(chats))
		for i, c := range chats {
			chatIDs[i] = c.ChatID
		}

		s.checkCert(now, settings, chatIDs, u, models.CertEgais, u.EgaisCertTo)
		s.checkCert(now, settings, chatIDs, u, models.CertGost, u.GostCertTo)
	}
}

func (s *Scheduler) checkCert(now time.Time, settings models.Settings, chatIDs []string, u models.UTM, certType models.CertType, expiry *time.Time) {
	if expiry == nil {
		return
	}
	daysLeft := int(expiry.Sub(now).Hours() / 24)
	if daysLeft < 0 {
		return
	}

	// Thresholds are checked from farthest to closest so that, if a УТМ was
	// just added or the app was offline for a while, all missed thresholds
	// up to the current one are sent (each still only once).
	for _, threshold := range models.NotificationThresholds {
		if daysLeft > threshold {
			continue
		}

		sent, err := s.store.NotificationAlreadySent(u.ID, certType, *expiry, threshold)
		if err != nil {
			log.Printf("notify: check sent state утм #%d: %v", u.ID, err)
			continue
		}
		if sent {
			continue
		}

		text := notificationText(u, certType, *expiry, daysLeft)
		for _, sendErr := range notifier.SendToAll(settings, chatIDs, text) {
			log.Printf("notify: %v", sendErr)
		}

		if err := s.store.MarkNotificationSent(u.ID, certType, *expiry, threshold); err != nil {
			log.Printf("notify: mark sent утм #%d: %v", u.ID, err)
		}
	}
}

// notificationText leads with whichever name most clearly identifies the
// client to a human reading several alerts in a row: the operator-chosen
// label first (it's what they intentionally called this site), falling
// back to the organization name fetched from the УТМ, then the IP.
func notificationText(u models.UTM, certType models.CertType, expiry time.Time, daysLeft int) string {
	orgName := models.PrettyOrgName(u.OrgName)
	name := u.Label
	if name == "" {
		name = orgName
	}
	if name == "" {
		name = u.IPAddress
	}

	who := name
	if orgName != "" && orgName != name {
		who += " (" + orgName + ")"
	}

	return fmt.Sprintf(
		"⚠️ %s\nУТМ %s: %s истекает %s (осталось %d дн.)",
		who, u.IPAddress, certType.Label(), expiry.Format("02.01.2006"), daysLeft,
	)
}

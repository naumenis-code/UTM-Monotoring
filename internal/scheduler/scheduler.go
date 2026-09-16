// Package scheduler drives periodic УТМ polling and expiry notifications.
package scheduler

import (
	"context"
	"fmt"
	"log"
	"strings"
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
// UI (see PollOneAsync). Every call utmclient makes is a local, synchronous
// HTTP request (see internal/utmclient) bounded by its own client timeout
// (config.PollHTTPTimeout, 10s) — this just covers the worst case of
// several such calls (primary + fallbacks) each timing out in sequence.
const backgroundPollTimeout = 60 * time.Second

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
// already sent for the certificate's current expiry date. Every item due in
// the same run is batched into a single digest message per recipient
// instead of one push notification per item — a recipient watching several
// УТМ (or the unscoped "all УТМ" ones) would otherwise get a burst of 5-10
// separate notifications whenever several certificates cross a threshold on
// the same poll. Each УТМ still only reaches its own scoped recipients plus
// the unscoped ones, so different clients' contacts never see each other's
// alerts. See dueThresholds for what happens when a certificate is found
// already expired (an outage spanning the expiry date) — it still gets
// exactly one alert rather than silence.
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
	linesByChat := make(map[string][]string)
	var toMark []sentKey

	for _, u := range utms {
		chats, err := s.store.ListTelegramChatsForUTM(u.ID)
		if err != nil {
			log.Printf("notify: list chats for утм #%d: %v", u.ID, err)
			continue
		}
		if len(chats) == 0 {
			continue
		}

		for _, cert := range []struct {
			certType models.CertType
			expiry   *time.Time
		}{
			{models.CertEgais, u.EgaisCertTo},
			{models.CertGost, u.GostCertTo},
		} {
			for _, due := range s.dueThresholds(u.ID, cert.certType, cert.expiry, now) {
				line := notificationLine(u, cert.certType, *cert.expiry, due.daysLeft, due.threshold)
				for _, c := range chats {
					linesByChat[c.ChatID] = append(linesByChat[c.ChatID], line)
				}
				toMark = append(toMark, sentKey{u.ID, cert.certType, *cert.expiry, due.threshold})
			}
		}
	}

	for chatID, lines := range linesByChat {
		if err := notifier.Send(settings, chatID, buildDigest(lines)); err != nil {
			log.Printf("notify: chat %s: %v", chatID, err)
		}
	}

	for _, k := range toMark {
		if err := s.store.MarkNotificationSent(k.utmID, k.certType, k.expiry, k.threshold); err != nil {
			log.Printf("notify: mark sent утм #%d: %v", k.utmID, err)
		}
	}
}

type sentKey struct {
	utmID     int64
	certType  models.CertType
	expiry    time.Time
	threshold int
}

type dueThreshold struct {
	threshold int
	daysLeft  int
}

// expiredThreshold is a sentinel "threshold" (not a real days-before-expiry
// value — none of models.NotificationThresholds is 0) used to mark the
// one-time "already expired and never alerted" notification. It reuses the
// same notifications_sent row shape without a schema change.
const expiredThreshold = 0

// dueThresholds returns, farthest-to-closest, every notification threshold
// that has been reached for this certificate and not already sent for its
// current expiry date — so if a УТМ was just added or the app was offline
// for a while, all missed thresholds up to the current one are sent (each
// still only once).
//
// If the certificate is discovered already expired (daysLeft < 0) — e.g.
// the dashboard was down across the expiry date and missed every countdown
// threshold — this does not stay silent: it sends one final "истёк" alert
// instead of replaying stale positive-day thresholds, and only once, so an
// outage never means the expiry passes without any notification at all.
func (s *Scheduler) dueThresholds(utmID int64, certType models.CertType, expiry *time.Time, now time.Time) []dueThreshold {
	if expiry == nil {
		return nil
	}
	daysLeft := int(expiry.Sub(now).Hours() / 24)

	if daysLeft < 0 {
		sent, err := s.store.NotificationAlreadySent(utmID, certType, *expiry, expiredThreshold)
		if err != nil {
			log.Printf("notify: check sent state утм #%d: %v", utmID, err)
			return nil
		}
		if sent {
			return nil
		}
		return []dueThreshold{{threshold: expiredThreshold, daysLeft: daysLeft}}
	}

	var due []dueThreshold
	for _, threshold := range models.NotificationThresholds {
		if daysLeft > threshold {
			continue
		}
		sent, err := s.store.NotificationAlreadySent(utmID, certType, *expiry, threshold)
		if err != nil {
			log.Printf("notify: check sent state утм #%d: %v", utmID, err)
			continue
		}
		if sent {
			continue
		}
		due = append(due, dueThreshold{threshold: threshold, daysLeft: daysLeft})
	}
	return due
}

// buildDigest joins one or more notificationLine entries into a single
// Telegram message with a short header, so several due certificates land in
// one push notification instead of a flood of separate ones.
func buildDigest(lines []string) string {
	return "⚠️ Истекают сертификаты УТМ:\n\n" + strings.Join(lines, "\n")
}

// notificationLine renders one due certificate as a single compact line —
// short enough to read cleanly on a phone without wrapping across several
// physical lines — leading with whichever name most clearly identifies the
// client: the operator-chosen label first (it's what they intentionally
// called this site), falling back to the organization name fetched from
// the УТМ, then the IP. The emoji encodes urgency at a glance when several
// lines are stacked in one digest. A negative daysLeft (the expiredThreshold
// catch-up case) reads as "истёк ... (N дн. назад)" rather than a confusing
// negative day count, always at the highest urgency.
func notificationLine(u models.UTM, certType models.CertType, expiry time.Time, daysLeft, threshold int) string {
	orgName := models.PrettyOrgName(u.OrgName)
	name := u.Label
	if name == "" {
		name = orgName
	}
	if name == "" {
		name = u.IPAddress
	}
	if orgName != "" && orgName != name {
		name += " (" + orgName + ")"
	}

	if daysLeft < 0 {
		return fmt.Sprintf("🚨 %s — %s истёк %s (%d дн. назад)",
			name, certType.ShortLabel(), expiry.Format("02.01.2006"), -daysLeft)
	}
	return fmt.Sprintf("%s %s — %s до %s, %d дн.",
		urgencyEmoji(threshold), name, certType.ShortLabel(), expiry.Format("02.01.2006"), daysLeft)
}

// urgencyEmoji maps a crossed threshold to a color that reads at a glance
// in a list of several lines — the closer the deadline, the hotter.
func urgencyEmoji(threshold int) string {
	switch {
	case threshold <= 1:
		return "🚨"
	case threshold <= 2:
		return "🔴"
	case threshold <= 5:
		return "🟠"
	case threshold <= 10:
		return "🟡"
	default:
		return "🔵"
	}
}

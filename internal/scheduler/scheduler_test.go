package scheduler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naumenis-code/UTM-Monotoring/internal/db"
	"github.com/naumenis-code/UTM-Monotoring/internal/models"
	"github.com/naumenis-code/UTM-Monotoring/internal/store"
	"github.com/naumenis-code/UTM-Monotoring/internal/utmclient"
)

// sentMessage is one call the fake Telegram relay received.
type sentMessage struct {
	ChatID string `json:"chat_id"`
	Text   string `json:"text"`
}

// newTestScheduler wires a real (temp-file) SQLite store to a Scheduler and a
// fake Telegram relay server, so CheckAndNotify exercises the exact same
// code path production uses (notifier.Send -> relay mode), just pointed at
// a local httptest.Server instead of a real relay.
func newTestScheduler(t *testing.T) (*Scheduler, *store.Store, *[]sentMessage) {
	t.Helper()

	var mu sync.Mutex
	var received []sentMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m sentMessage
		_ = json.Unmarshal(body, &m)
		mu.Lock()
		received = append(received, m)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	sqlDB, err := db.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })

	st := store.New(sqlDB)
	if err := st.UpdateSettings(models.Settings{
		PollTime1:            "09:00",
		TelegramBotToken:     "TESTTOKEN",
		TelegramMode:         models.TelegramModeRelay,
		TelegramRelayBaseURL: srv.URL,
		TelegramRelayAuthKey: "TESTKEY",
	}); err != nil {
		t.Fatalf("update settings: %v", err)
	}

	sched := New(st, utmclient.New(time.Second))
	return sched, st, &received
}

// createUTM registers a УТМ with the given ЕГАИС/ГОСТ expiry dates (either
// may be nil) and returns its id.
func createUTM(t *testing.T, st *store.Store, label string, egaisTo, gostTo *time.Time) int64 {
	t.Helper()
	id, err := st.CreateUTM(label, "10.0.0."+label, 8080)
	if err != nil {
		t.Fatalf("create utm: %v", err)
	}
	if err := st.UpdateUTMMeta(id, label, 8080, "", "", "", "", nil, egaisTo, nil, gostTo); err != nil {
		t.Fatalf("update utm meta: %v", err)
	}
	return id
}

func daysFromNow(d int) *time.Time {
	t := time.Now().UTC().Add(time.Duration(d) * 24 * time.Hour)
	return &t
}

// TestCheckAndNotify_SentOnceOnly verifies the core "won't repeat" guarantee:
// a threshold that has already fired for a certificate's current expiry
// date is never sent again, no matter how many more times CheckAndNotify
// runs (mirroring twice-daily polling).
func TestCheckAndNotify_SentOnceOnly(t *testing.T) {
	sched, st, sent := newTestScheduler(t)

	utmID := createUTM(t, st, "1", daysFromNow(29), nil)
	if err := st.AddTelegramChat("111", "admin", nil); err != nil {
		t.Fatalf("add chat: %v", err)
	}

	sched.CheckAndNotify(context.Background())
	if len(*sent) != 1 {
		t.Fatalf("after first run: got %d messages, want 1", len(*sent))
	}
	if !strings.Contains((*sent)[0].Text, "ЕГАИС") {
		t.Errorf("message text = %q, want it to mention ЕГАИС", (*sent)[0].Text)
	}

	// Simulate the evening poll (same day) and several more polls on later
	// days that don't cross a new threshold: must not resend.
	sched.CheckAndNotify(context.Background())
	sched.CheckAndNotify(context.Background())
	if len(*sent) != 1 {
		t.Fatalf("after repeated runs with no new threshold crossed: got %d messages, want still 1", len(*sent))
	}
	_ = utmID
}

// TestCheckAndNotify_BatchesMultipleDueItemsInOneMessage verifies that when
// several УТМ cross a threshold on the same run, one recipient gets exactly
// one combined message listing all of them (not a flood of separate ones),
// each with its own correct data.
func TestCheckAndNotify_BatchesMultipleDueItemsInOneMessage(t *testing.T) {
	sched, st, sent := newTestScheduler(t)

	createUTM(t, st, "A", daysFromNow(29), nil) // crosses 30
	createUTM(t, st, "B", nil, daysFromNow(9))  // crosses 10
	createUTM(t, st, "C", daysFromNow(1), nil)  // crosses 2 and 1
	if err := st.AddTelegramChat("222", "admin", nil); err != nil {
		t.Fatalf("add chat: %v", err)
	}

	sched.CheckAndNotify(context.Background())

	if len(*sent) != 1 {
		t.Fatalf("got %d messages, want exactly 1 combined digest", len(*sent))
	}
	text := (*sent)[0].Text
	lineCount := strings.Count(text, "\n") - 1 // header line + blank line before items
	for _, want := range []string{"A", "B", "C", "ЕГАИС", "ГОСТ"} {
		if !strings.Contains(text, want) {
			t.Errorf("digest missing %q; got:\n%s", want, text)
		}
	}
	// A and C each cross exactly one threshold this run (30, and 1 — note C's
	// daysLeft=1 only satisfies the closest uncrossed threshold since it's
	// the first run), B crosses one (10): expect at least 3 item lines.
	if lineCount < 3 {
		t.Errorf("expected at least 3 item lines in digest, got %d; text:\n%s", lineCount, text)
	}
}

// TestCheckAndNotify_RecipientScoping verifies a recipient scoped to one
// УТМ never sees another client's alert, while the unscoped ("all УТМ")
// recipient sees everything — the whole point of per-client scoping.
func TestCheckAndNotify_RecipientScoping(t *testing.T) {
	sched, st, sent := newTestScheduler(t)

	utmA := createUTM(t, st, "A", daysFromNow(1), nil)
	createUTM(t, st, "B", daysFromNow(1), nil)

	if err := st.AddTelegramChat("scoped-to-a", "client A contact", &utmA); err != nil {
		t.Fatalf("add scoped chat: %v", err)
	}
	if err := st.AddTelegramChat("admin-all", "admin", nil); err != nil {
		t.Fatalf("add unscoped chat: %v", err)
	}

	sched.CheckAndNotify(context.Background())

	byChat := map[string]string{}
	for _, m := range *sent {
		byChat[m.ChatID] = m.Text
	}
	if len(byChat) != 2 {
		t.Fatalf("got messages for %d chats, want 2", len(byChat))
	}
	if strings.Contains(byChat["scoped-to-a"], "B") {
		t.Errorf("chat scoped to УТМ A saw УТМ B's alert: %q", byChat["scoped-to-a"])
	}
	if !strings.Contains(byChat["scoped-to-a"], "A") {
		t.Errorf("chat scoped to УТМ A did not see its own alert: %q", byChat["scoped-to-a"])
	}
	if !strings.Contains(byChat["admin-all"], "A") || !strings.Contains(byChat["admin-all"], "B") {
		t.Errorf("unscoped admin chat should see both А and B: %q", byChat["admin-all"])
	}
}

// TestCheckAndNotify_ExpiredCertStillGetsOneAlert covers the gap this test
// suite was written to close: a certificate discovered already expired
// (e.g. the dashboard was offline across its expiry date, missing every
// countdown threshold) must still produce exactly one alert, not silence.
func TestCheckAndNotify_ExpiredCertStillGetsOneAlert(t *testing.T) {
	sched, st, sent := newTestScheduler(t)

	createUTM(t, st, "1", daysFromNow(-5), nil)
	if err := st.AddTelegramChat("111", "admin", nil); err != nil {
		t.Fatalf("add chat: %v", err)
	}

	sched.CheckAndNotify(context.Background())
	if len(*sent) != 1 {
		t.Fatalf("got %d messages for a newly-discovered expired cert, want exactly 1", len(*sent))
	}
	text := (*sent)[0].Text
	if !strings.Contains(text, "истёк") {
		t.Errorf("expired-cert message should say истёк, got: %q", text)
	}
	if !strings.Contains(text, "5 дн. назад") {
		t.Errorf("expired-cert message should show days-since-expiry, got: %q", text)
	}

	// Must not repeat on subsequent runs.
	sched.CheckAndNotify(context.Background())
	if len(*sent) != 1 {
		t.Fatalf("expired-cert alert repeated: got %d messages, want still 1", len(*sent))
	}
}

// TestCheckAndNotify_RenewalResetsSchedule verifies that once a certificate
// is renewed (its expiry date changes), the notification schedule for the
// new date starts fresh rather than staying suppressed by the old date's
// history.
func TestCheckAndNotify_RenewalResetsSchedule(t *testing.T) {
	sched, st, sent := newTestScheduler(t)

	id := createUTM(t, st, "1", daysFromNow(1), nil)
	if err := st.AddTelegramChat("111", "admin", nil); err != nil {
		t.Fatalf("add chat: %v", err)
	}

	sched.CheckAndNotify(context.Background())
	if len(*sent) != 1 {
		t.Fatalf("got %d messages before renewal, want 1", len(*sent))
	}

	// Renew: push the expiry a year out.
	if err := st.UpdateUTMMeta(id, "1", 8080, "", "", "", "", nil, daysFromNow(365), nil, nil); err != nil {
		t.Fatalf("renew: %v", err)
	}
	sched.CheckAndNotify(context.Background())
	if len(*sent) != 1 {
		t.Fatalf("got %d messages right after renewal (365 days left, no threshold due), want still 1", len(*sent))
	}
}

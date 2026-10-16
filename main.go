package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html/template"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Config holds all environment-driven settings.
type Config struct {
	DBPath          string
	ListenAddr      string
	AthleteName     string
	DreeveURL       string
	DreeveToken     string
	PublicBaseURL   string
	HMACSecret      string
	PollInterval    time.Duration
	DigestWeekday   time.Weekday
	DigestHour      int
	MailgunDomain   string
	MailgunAPIKey   string
	MailgunFrom     string
	MailgunAPIBase  string
	NtfyTopicURL    string
	NtfyToken       string
	InitialFriends  string
	SeedExisting    bool
}

type App struct {
	cfg        Config
	db         *sql.DB
	httpClient *http.Client
}

// DreeveActivity matches GET /api/v1/activities items from docs.dreeve.app/api/
type DreeveActivity struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	SportType      string `json:"sportType"`
	WorldType      string `json:"worldType"`
	StartDateLocal string `json:"startDateLocal"`
	HasGPX         bool   `json:"hasGpx"`
	Location       struct {
		CountryCode *string `json:"countryCode"`
		State       *string `json:"state"`
		City        *string `json:"city"`
	} `json:"location"`
	Images []struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	} `json:"images"`
	// Optional numeric fields if returned by Dreeve or computed from GPX
	DistanceMeters     float64 `json:"distance"`
	MovingTimeSeconds  int     `json:"movingTime"`
	ElevationGainMeter float64 `json:"elevationGain"`
}

type StoredActivity struct {
	ID             string
	Name           string
	SportType      string
	StartDateLocal string
	LocationStr    string
	DistanceM      float64
	MovingTimeS    int
	ElevationM     float64
	ImageURL       string
	DigestSent     bool
	CreatedAt      time.Time
}

type Subscriber struct {
	ID    string // URL-safe slug, e.g. "alice"
	Name  string // Display name, e.g. "Alice"
	Email string
	Pref  string // "instant", "weekly", or "none"
}

func loadConfig() Config {
	pollMin, err := strconv.Atoi(getEnv("POLL_INTERVAL_MINUTES", "5"))
	if err != nil || pollMin < 1 {
		pollMin = 5
	}
	digestDayInt, err := strconv.Atoi(getEnv("DIGEST_WEEKDAY", "0")) // 0 = Sunday
	if err != nil || digestDayInt < 0 || digestDayInt > 6 {
		digestDayInt = 0
	}
	digestHour, err := strconv.Atoi(getEnv("DIGEST_HOUR", "18")) // 18:00 (6 PM)
	if err != nil || digestHour < 0 || digestHour > 23 {
		digestHour = 18
	}

	return Config{
		DBPath:         getEnv("DB_PATH", "/data/sidecar.db"),
		ListenAddr:     getEnv("LISTEN_ADDR", ":8080"),
		AthleteName:    getEnv("ATHLETE_NAME", "Alex"),
		DreeveURL:      strings.TrimRight(getEnv("DREEVE_URL", "http://dreeve:8080"), "/"),
		DreeveToken:    getEnv("DREEVE_API_TOKEN", ""),
		PublicBaseURL:  strings.TrimRight(getEnv("PUBLIC_BASE_URL", "http://localhost:8090"), "/"),
		HMACSecret:     getEnv("HMAC_SECRET", "change-me-in-production"),
		PollInterval:   time.Duration(pollMin) * time.Minute,
		DigestWeekday:  time.Weekday(digestDayInt),
		DigestHour:     digestHour,
		MailgunDomain:  getEnv("MAILGUN_DOMAIN", ""),
		MailgunAPIKey:  getEnv("MAILGUN_API_KEY", ""),
		MailgunFrom:    getEnv("MAILGUN_FROM", "Dreeve Social <noreply@example.com>"),
		MailgunAPIBase: strings.TrimRight(getEnv("MAILGUN_API_BASE", "https://api.mailgun.net"), "/"),
		NtfyTopicURL:   getEnv("NTFY_TOPIC_URL", ""),
		NtfyToken:      getEnv("NTFY_TOKEN", ""),
		InitialFriends: getEnv("FRIENDS", ""),
		SeedExisting:   strings.EqualFold(getEnv("SEED_EXISTING_WITHOUT_EMAIL", "true"), "true"),
	}
}

func getEnv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func main() {
	cfg := loadConfig()
	db, err := initDB(cfg.DBPath)
	if err != nil {
		log.Fatalf("failed to initialize sqlite db: %v", err)
	}
	defer db.Close()

	app := &App{
		cfg:        cfg,
		db:         db,
		httpClient: &http.Client{Timeout: 25 * time.Second},
	}

	if err := app.seedFriendsFromEnv(); err != nil {
		log.Printf("warning seeding friends: %v", err)
	}

	// Background goroutine: Dreeve poller + Weekly Digest scheduler
	go app.backgroundLoop()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /feed.xml", app.handleRSSFeed)
	mux.HandleFunc("GET /kudos", app.handleKudosGet)
	mux.HandleFunc("POST /api/kudos", app.handleKudosPost)
	mux.HandleFunc("GET /preferences", app.handlePreferencesGet)
	mux.HandleFunc("POST /preferences", app.handlePreferencesPost)

	log.Printf("dreeve-social listening on %s (public URL: %s)", cfg.ListenAddr, cfg.PublicBaseURL)
	log.Fatal(http.ListenAndServe(cfg.ListenAddr, mux))
}

// ============================================================================
// Database Initialization & Friend Seeding
// ============================================================================

func initDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	schema := `
	CREATE TABLE IF NOT EXISTS subscribers (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		email TEXT NOT NULL,
		pref TEXT NOT NULL DEFAULT 'instant',
		updated_at DATETIME NOT NULL
	);

	CREATE TABLE IF NOT EXISTS activities (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		sport_type TEXT NOT NULL,
		start_date_local TEXT NOT NULL,
		location_str TEXT NOT NULL,
		distance_m REAL NOT NULL DEFAULT 0,
		moving_time_s INTEGER NOT NULL DEFAULT 0,
		elevation_m REAL NOT NULL DEFAULT 0,
		image_url TEXT NOT NULL DEFAULT '',
		digest_sent INTEGER NOT NULL DEFAULT 0,
		created_at DATETIME NOT NULL
	);

	CREATE TABLE IF NOT EXISTS kudos (
		activity_id TEXT NOT NULL,
		subscriber_id TEXT NOT NULL,
		subscriber_name TEXT NOT NULL,
		created_at DATETIME NOT NULL,
		PRIMARY KEY (activity_id, subscriber_id)
	);

	CREATE TABLE IF NOT EXISTS meta (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);
	`
	_, err = db.Exec(schema)
	return db, err
}

// seedFriendsFromEnv parses FRIENDS="alice:Alice:alice@example.com:instant,bob:Bob:bob@example.com:weekly"
// or shorthand "alice:alice@example.com,bob:bob@example.com".
// It inserts new subscribers and updates email/name, but NEVER overwrites a friend's chosen 'pref' if they already exist.
func (a *App) seedFriendsFromEnv() error {
	if a.cfg.InitialFriends == "" {
		return nil
	}
	entries := strings.Split(a.cfg.InitialFriends, ",")
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.Split(entry, ":")
		var id, name, email, pref string
		switch len(parts) {
		case 2:
			id = strings.ToLower(strings.TrimSpace(parts[0]))
			name = strings.Title(id)
			email = strings.TrimSpace(parts[1])
			pref = "instant"
		case 3:
			id = strings.ToLower(strings.TrimSpace(parts[0]))
			name = strings.TrimSpace(parts[0])
			email = strings.TrimSpace(parts[1])
			pref = strings.ToLower(strings.TrimSpace(parts[2]))
		case 4:
			id = strings.ToLower(strings.TrimSpace(parts[0]))
			name = strings.TrimSpace(parts[1])
			email = strings.TrimSpace(parts[2])
			pref = strings.ToLower(strings.TrimSpace(parts[3]))
		default:
			log.Printf("skipping malformed FRIENDS entry: %q", entry)
			continue
		}
		if pref != "instant" && pref != "weekly" && pref != "none" {
			pref = "instant"
		}
		_, err := a.db.Exec(`
			INSERT INTO subscribers (id, name, email, pref, updated_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				name = excluded.name,
				email = excluded.email
		`, id, name, email, pref, time.Now().UTC())
		if err != nil {
			return err
		}
		log.Printf("subscriber ready: %s (%s) | Manage & RSS URL: %s",
			name, email, a.prefURL(id))
	}
	return nil
}

// ============================================================================
// HMAC Signing & Personalized URL Builders
// ============================================================================

func (a *App) sign(purpose, payload string) string {
	mac := hmac.New(sha256.New, []byte(a.cfg.HMACSecret))
	mac.Write([]byte(purpose + ":" + payload))
	return hex.EncodeToString(mac.Sum(nil))
}

func (a *App) verify(purpose, payload, sig string) bool {
	expected := a.sign(purpose, payload)
	return hmac.Equal([]byte(expected), []byte(sig))
}

func (a *App) kudosURL(activityID, subscriberID string) string {
	sig := a.sign("kudos", activityID+":"+subscriberID)
	q := url.Values{}
	q.Set("act", activityID)
	q.Set("who", subscriberID)
	q.Set("sig", sig)
	return a.cfg.PublicBaseURL + "/kudos?" + q.Encode()
}

func (a *App) prefURL(subscriberID string) string {
	sig := a.sign("pref", subscriberID)
	q := url.Values{}
	q.Set("who", subscriberID)
	q.Set("sig", sig)
	return a.cfg.PublicBaseURL + "/preferences?" + q.Encode()
}

func (a *App) feedURL(subscriberID, mode string) string {
	sig := a.sign("feed", subscriberID+":"+mode)
	q := url.Values{}
	q.Set("who", subscriberID)
	q.Set("mode", mode)
	q.Set("sig", sig)
	return a.cfg.PublicBaseURL + "/feed.xml?" + q.Encode()
}

// ============================================================================
// Dreeve Polling & Weekly Digest Loop
// ============================================================================

func (a *App) backgroundLoop() {
	a.pollDreeve()
	a.maybeSendWeeklyDigests()

	pollTicker := time.NewTicker(a.cfg.PollInterval)
	digestTicker := time.NewTicker(15 * time.Minute)
	defer pollTicker.Stop()
	defer digestTicker.Stop()

	for {
		select {
		case <-pollTicker.C:
			a.pollDreeve()
		case <-digestTicker.C:
			a.maybeSendWeeklyDigests()
		}
	}
}

func (a *App) pollDreeve() {
	if a.cfg.DreeveToken == "" {
		log.Println("DREEVE_API_TOKEN not set; skipping poll")
		return
	}

	req, err := http.NewRequest("GET", a.cfg.DreeveURL+"/api/v1/activities", nil)
	if err != nil {
		log.Printf("poll request build error: %v", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.DreeveToken)
	req.Header.Set("Accept", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		log.Printf("poll error contacting Dreeve: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		log.Printf("Dreeve returned status %d: %s", resp.StatusCode, string(body))
		return
	}

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("error reading Dreeve response: %v", err)
		return
	}

	activities, err := parseDreeveActivities(rawBody)
	if err != nil {
		log.Printf("error decoding Dreeve activities JSON: %v", err)
		return
	}

	// Check if this is the very first run on an empty DB so we don't spam 50 old emails
	isFirstSync := a.isFirstSync()

	// Process oldest-to-newest
	for i := len(activities) - 1; i >= 0; i-- {
		act := activities[i]
		if act.ID == "" {
			continue
		}
		if a.activityExists(act.ID) {
			continue
		}

		// Enrich distance & elevation from GPX if not in summary JSON
		if act.DistanceMeters == 0 && act.HasGPX {
			distM, elevM, durS := a.computeStatsFromGPX(act.ID)
			if distM > 0 {
				act.DistanceMeters = distM
			}
			if elevM > 0 {
				act.ElevationGainMeter = elevM
			}
			if durS > 0 && act.MovingTimeSeconds == 0 {
				act.MovingTimeSeconds = durS
			}
		}

		stored := toStoredActivity(act)
		if isFirstSync && a.cfg.SeedExisting {
			stored.DigestSent = true
		}

		if err := a.saveActivity(stored); err != nil {
			log.Printf("failed saving activity %s: %v", stored.ID, err)
			continue
		}

		if isFirstSync && a.cfg.SeedExisting {
			log.Printf("seeded historical activity %s (%s) without emailing", stored.ID, stored.Name)
			continue
		}

		log.Printf("new activity detected: %s (%s)", stored.Name, stored.ID)
		a.sendInstantEmails(stored)
	}

	if isFirstSync {
		_, _ = a.db.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('initial_sync_done', '1')`)
	}
}

func parseDreeveActivities(raw []byte) ([]DreeveActivity, error) {
	// Support both direct JSON array [...] and wrapped object {"items": [...]} or {"data": [...]}
	var list []DreeveActivity
	if err := json.Unmarshal(raw, &list); err == nil {
		return list, nil
	}
	var wrapped struct {
		Items      []DreeveActivity `json:"items"`
		Data       []DreeveActivity `json:"data"`
		Activities []DreeveActivity `json:"activities"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return nil, err
	}
	if len(wrapped.Items) > 0 {
		return wrapped.Items, nil
	}
	if len(wrapped.Data) > 0 {
		return wrapped.Data, nil
	}
	return wrapped.Activities, nil
}

// computeStatsFromGPX fetches GET /api/v1/activities/{id}/gpx and computes Haversine distance & elevation gain.
func (a *App) computeStatsFromGPX(activityID string) (distMeters float64, elevGain float64, durationSec int) {
	req, err := http.NewRequest("GET", a.cfg.DreeveURL+"/api/v1/activities/"+url.PathEscape(activityID)+"/gpx", nil)
	if err != nil {
		return 0, 0, 0
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.DreeveToken)
	resp, err := a.httpClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return 0, 0, 0
	}
	defer resp.Body.Close()

	var gpx struct {
		Trk []struct {
			TrkSeg []struct {
				TrkPt []struct {
					Lat  float64 `xml:"lat,attr"`
					Lon  float64 `xml:"lon,attr"`
					Ele  float64 `xml:"ele"`
					Time string  `xml:"time"`
				} `xml:"trkpt"`
			} `xml:"trkseg"`
		} `xml:"trk"`
	}
	if err := xml.NewDecoder(resp.Body).Decode(&gpx); err != nil {
		return 0, 0, 0
	}

	var prevLat, prevLon, prevEle float64
	var havePrev bool
	var firstTime, lastTime time.Time

	for _, trk := range gpx.Trk {
		for _, seg := range trk.TrkSeg {
			for _, pt := range seg.TrkPt {
				if pt.Time != "" {
					if t, err := time.Parse(time.RFC3339, pt.Time); err == nil {
						if firstTime.IsZero() {
							firstTime = t
						}
						lastTime = t
					}
				}
				if havePrev {
					distMeters += haversineMeters(prevLat, prevLon, pt.Lat, pt.Lon)
					if diff := pt.Ele - prevEle; diff > 0.5 {
						elevGain += diff
					}
				}
				prevLat, prevLon, prevEle = pt.Lat, pt.Lon, pt.Ele
				havePrev = true
			}
		}
	}
	if !firstTime.IsZero() && !lastTime.IsZero() && lastTime.After(firstTime) {
		durationSec = int(lastTime.Sub(firstTime).Seconds())
	}
	return distMeters, elevGain, durationSec
}

func haversineMeters(lat1, lon1, lat2, lon2 float64) float64 {
	const R = 6371000.0
	dLat := (lat2 - lat1) * math.Pi / 180.0
	dLon := (lon2 - lon1) * math.Pi / 180.0
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*math.Pi/180.0)*math.Cos(lat2*math.Pi/180.0)*
			math.Sin(dLon/2)*math.Sin(dLon/2)
	return R * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

func toStoredActivity(act DreeveActivity) StoredActivity {
	var locParts []string
	if act.Location.City != nil && *act.Location.City != "" {
		locParts = append(locParts, *act.Location.City)
	}
	if act.Location.State != nil && *act.Location.State != "" {
		locParts = append(locParts, *act.Location.State)
	}
	if act.Location.CountryCode != nil && *act.Location.CountryCode != "" {
		locParts = append(locParts, strings.ToUpper(*act.Location.CountryCode))
	}
	imgURL := ""
	if len(act.Images) > 0 {
		imgURL = act.Images[0].URL
	}
	return StoredActivity{
		ID:             act.ID,
		Name:           act.Name,
		SportType:      act.SportType,
		StartDateLocal: act.StartDateLocal,
		LocationStr:    strings.Join(locParts, ", "),
		DistanceM:      act.DistanceMeters,
		MovingTimeS:    act.MovingTimeSeconds,
		ElevationM:     act.ElevationGainMeter,
		ImageURL:       imgURL,
		CreatedAt:      time.Now().UTC(),
	}
}

func (a *App) isFirstSync() bool {
	var val string
	err := a.db.QueryRow(`SELECT value FROM meta WHERE key = 'initial_sync_done'`).Scan(&val)
	return err != nil
}

func (a *App) activityExists(id string) bool {
	var existing string
	err := a.db.QueryRow(`SELECT id FROM activities WHERE id = ?`, id).Scan(&existing)
	return err == nil
}

func (a *App) saveActivity(act StoredActivity) error {
	digestInt := 0
	if act.DigestSent {
		digestInt = 1
	}
	_, err := a.db.Exec(`
		INSERT INTO activities (
			id, name, sport_type, start_date_local, location_str,
			distance_m, moving_time_s, elevation_m, image_url, digest_sent, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, act.ID, act.Name, act.SportType, act.StartDateLocal, act.LocationStr,
		act.DistanceM, act.MovingTimeS, act.ElevationM, act.ImageURL, digestInt, act.CreatedAt)
	return err
}

// ============================================================================
// Mailgun Email Delivery (Instant & Weekly Digest)
// ============================================================================

func (a *App) sendInstantEmails(act StoredActivity) {
	subs, err := a.listSubscribersByPref("instant")
	if err != nil {
		log.Printf("error querying instant subscribers: %v", err)
		return
	}
	for _, sub := range subs {
		subject := fmt.Sprintf("🏅 %s logged a %s: %s", a.cfg.AthleteName, act.SportType, act.Name)
		htmlBody, textBody, err := a.renderActivityEmail(sub, []StoredActivity{act}, false)
		if err != nil {
			log.Printf("template error for %s: %v", sub.Email, err)
			continue
		}
		if err := a.sendMailgun(sub.Email, subject, textBody, htmlBody); err != nil {
			log.Printf("mailgun error sending instant email to %s: %v", sub.Email, err)
		} else {
			log.Printf("sent instant activity email for %s to %s", act.ID, sub.Email)
		}
	}
}

func (a *App) maybeSendWeeklyDigests() {
	now := time.Now()
	if now.Weekday() != a.cfg.DigestWeekday || now.Hour() < a.cfg.DigestHour {
		return
	}

	// Ensure we only send one weekly digest per ISO week
	isoYear, isoWeek := now.ISOWeek()
	weekKey := fmt.Sprintf("%d-W%02d", isoYear, isoWeek)
	var lastSentWeek string
	_ = a.db.QueryRow(`SELECT value FROM meta WHERE key = 'last_digest_week'`).Scan(&lastSentWeek)
	if lastSentWeek == weekKey {
		return
	}

	pending, err := a.getPendingDigestActivities()
	if err != nil || len(pending) == 0 {
		return
	}

	subs, err := a.listSubscribersByPref("weekly")
	if err != nil {
		log.Printf("error querying weekly subscribers: %v", err)
		return
	}

	for _, sub := range subs {
		subject := fmt.Sprintf("📅 %s's Weekly Activity Digest (%d workouts)", a.cfg.AthleteName, len(pending))
		htmlBody, textBody, err := a.renderActivityEmail(sub, pending, true)
		if err != nil {
			log.Printf("digest template error for %s: %v", sub.Email, err)
			continue
		}
		if err := a.sendMailgun(sub.Email, subject, textBody, htmlBody); err != nil {
			log.Printf("mailgun error sending weekly digest to %s: %v", sub.Email, err)
		} else {
			log.Printf("sent weekly digest (%d activities) to %s", len(pending), sub.Email)
		}
	}

	// Mark activities as included in weekly digest and record weekKey
	_, _ = a.db.Exec(`UPDATE activities SET digest_sent = 1 WHERE digest_sent = 0`)
	_, _ = a.db.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('last_digest_week', ?)`, weekKey)
}

func (a *App) sendMailgun(to, subject, textBody, htmlBody string) error {
	if a.cfg.MailgunDomain == "" || a.cfg.MailgunAPIKey == "" {
		log.Printf("[DRY-RUN EMAIL] To=%s | Subject=%s (Set MAILGUN_DOMAIN & MAILGUN_API_KEY to send)", to, subject)
		return nil
	}

	form := url.Values{}
	form.Set("from", a.cfg.MailgunFrom)
	form.Set("to", to)
	form.Set("subject", subject)
	form.Set("text", textBody)
	form.Set("html", htmlBody)

	endpoint := fmt.Sprintf("%s/v3/%s/messages", a.cfg.MailgunAPIBase, a.cfg.MailgunDomain)
	req, err := http.NewRequestWithContext(context.Background(), "POST", endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.SetBasicAuth("api", a.cfg.MailgunAPIKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("mailgun HTTP %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

type emailActivityCard struct {
	StoredActivity
	FormattedDate string
	StatsLine     string
	KudosURL      string
}

func (a *App) renderActivityEmail(sub Subscriber, acts []StoredActivity, isWeekly bool) (string, string, error) {
	cards := make([]emailActivityCard, 0, len(acts))
	var textBuilder strings.Builder

	if isWeekly {
		fmt.Fprintf(&textBuilder, "Hi %s,\n\nHere is %s's weekly workout digest:\n\n", sub.Name, a.cfg.AthleteName)
	} else {
		fmt.Fprintf(&textBuilder, "Hi %s,\n\n%s just finished a new workout:\n\n", sub.Name, a.cfg.AthleteName)
	}

	for _, act := range acts {
		kURL := a.kudosURL(act.ID, sub.ID)
		stats := formatStatsLine(act)
		fDate := formatPrettyDate(act.StartDateLocal)
		cards = append(cards, emailActivityCard{
			StoredActivity: act,
			FormattedDate:  fDate,
			StatsLine:      stats,
			KudosURL:       kURL,
		})
		fmt.Fprintf(&textBuilder, "- %s (%s) on %s\n", act.Name, act.SportType, fDate)
		if stats != "" {
			fmt.Fprintf(&textBuilder, "  Stats: %s\n", stats)
		}
		if act.LocationStr != "" {
			fmt.Fprintf(&textBuilder, "  Location: %s\n", act.LocationStr)
		}
		fmt.Fprintf(&textBuilder, "  Give Kudos (1-click): %s\n\n", kURL)
	}

	pURL := a.prefURL(sub.ID)
	instantRSS := a.feedURL(sub.ID, "instant")
	weeklyRSS := a.feedURL(sub.ID, "weekly")

	fmt.Fprintf(&textBuilder, "---\nManage email frequency, unsubscribe, or re-subscribe anytime:\n%s\n\nYour personal RSS feeds:\n- Per-Activity RSS: %s\n- Weekly Digest RSS: %s\n",
		pURL, instantRSS, weeklyRSS)

	data := struct {
		SubscriberName string
		AthleteName    string
		IsWeekly       bool
		Cards          []emailActivityCard
		PrefURL        string
		InstantRSSURL  string
		WeeklyRSSURL   string
	}{
		SubscriberName: sub.Name,
		AthleteName:    a.cfg.AthleteName,
		IsWeekly:       isWeekly,
		Cards:          cards,
		PrefURL:        pURL,
		InstantRSSURL:  instantRSS,
		WeeklyRSSURL:   weeklyRSS,
	}

	var htmlBuf bytes.Buffer
	if err := emailTmpl.Execute(&htmlBuf, data); err != nil {
		return "", "", err
	}
	return htmlBuf.String(), textBuilder.String(), nil
}

// ============================================================================
// HTTP Handlers: Preferences / Unsubscribe / Re-subscribe & RSS Feeds
// ============================================================================

func (a *App) handlePreferencesGet(w http.ResponseWriter, r *http.Request) {
	who := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("who")))
	sig := r.URL.Query().Get("sig")
	if who == "" || !a.verify("pref", who, sig) {
		http.Error(w, "Invalid or expired preference signature", http.StatusForbidden)
		return
	}

	sub, err := a.getSubscriber(who)
	if err != nil {
		http.Error(w, "Subscriber not found", http.StatusNotFound)
		return
	}

	a.renderPreferencesPage(w, sub, sig, r.URL.Query().Get("saved") == "1")
}

func (a *App) handlePreferencesPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form submission", http.StatusBadRequest)
		return
	}
	who := strings.ToLower(strings.TrimSpace(r.FormValue("who")))
	sig := r.FormValue("sig")
	pref := strings.ToLower(strings.TrimSpace(r.FormValue("pref")))

	if who == "" || !a.verify("pref", who, sig) {
		http.Error(w, "Invalid or expired preference signature", http.StatusForbidden)
		return
	}
	if pref != "instant" && pref != "weekly" && pref != "none" {
		http.Error(w, "Invalid preference option", http.StatusBadRequest)
		return
	}

	_, err := a.db.Exec(`UPDATE subscribers SET pref = ?, updated_at = ? WHERE id = ?`,
		pref, time.Now().UTC(), who)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, a.prefURL(who)+"&saved=1", http.StatusSeeOther)
}

func (a *App) renderPreferencesPage(w http.ResponseWriter, sub Subscriber, sig string, saved bool) {
	data := struct {
		Subscriber    Subscriber
		AthleteName   string
		Sig           string
		Saved         bool
		InstantRSSURL string
		WeeklyRSSURL  string
	}{
		Subscriber:    sub,
		AthleteName:   a.cfg.AthleteName,
		Sig:           sig,
		Saved:         saved,
		InstantRSSURL: a.feedURL(sub.ID, "instant"),
		WeeklyRSSURL:  a.feedURL(sub.ID, "weekly"),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = prefPageTmpl.Execute(w, data)
}

// handleRSSFeed serves user-specific RSS feeds in either 'instant' (per-activity) or 'weekly' (digest) mode.
func (a *App) handleRSSFeed(w http.ResponseWriter, r *http.Request) {
	who := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("who")))
	mode := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("mode")))
	sig := r.URL.Query().Get("sig")

	if mode != "weekly" {
		mode = "instant"
	}
	if who == "" || !a.verify("feed", who+":"+mode, sig) {
		http.Error(w, "Invalid RSS feed signature. Use the personal RSS link from your email or preferences page.", http.StatusForbidden)
		return
	}

	sub, err := a.getSubscriber(who)
	if err != nil {
		http.Error(w, "Subscriber not found", http.StatusNotFound)
		return
	}

	acts, err := a.listRecentActivities(50)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	type rssItem struct {
		Title       string `xml:"title"`
		Link        string `xml:"link"`
		GUID        string `xml:"guid"`
		PubDate     string `xml:"pubDate"`
		Description string `xml:"description"`
	}
	type rssChannel struct {
		Title       string    `xml:"title"`
		Link        string    `xml:"link"`
		Description string    `xml:"description"`
		Items       []rssItem `xml:"item"`
	}
	type rssRoot struct {
		XMLName xml.Name   `xml:"rss"`
		Version string     `xml:"version,attr"`
		Channel rssChannel `xml:"channel"`
	}

	var items []rssItem
	if mode == "instant" {
		for _, act := range acts {
			actTime := parseActivityTime(act.StartDateLocal, act.CreatedAt)
			kURL := a.kudosURL(act.ID, sub.ID)
			desc := fmt.Sprintf("<p><strong>%s</strong> (%s) — %s</p>",
				template.HTMLEscapeString(act.Name),
				template.HTMLEscapeString(act.SportType),
				template.HTMLEscapeString(formatPrettyDate(act.StartDateLocal)))
			if stats := formatStatsLine(act); stats != "" {
				desc += fmt.Sprintf("<p>📊 %s</p>", template.HTMLEscapeString(stats))
			}
			if act.LocationStr != "" {
				desc += fmt.Sprintf("<p>📍 %s</p>", template.HTMLEscapeString(act.LocationStr))
			}
			if act.ImageURL != "" {
				desc += fmt.Sprintf(`<p><img src="%s" alt="Workout photo" style="max-width:100%%;border-radius:8px;" /></p>`,
					template.HTMLEscapeString(act.ImageURL))
			}
			desc += fmt.Sprintf(`<p><a href="%s"><strong>👍 Give Kudos to %s (1-Click)</strong></a></p>`,
				template.HTMLEscapeString(kURL), template.HTMLEscapeString(a.cfg.AthleteName))

			items = append(items, rssItem{
				Title:       fmt.Sprintf("%s: %s", act.SportType, act.Name),
				Link:        kURL,
				GUID:        act.ID + "-" + sub.ID,
				PubDate:     actTime.Format(time.RFC1123Z),
				Description: desc,
			})
		}
	} else {
		// Group activities by ISO week for the weekly digest RSS feed
		type weekGroup struct {
			Key        string
			LatestTime time.Time
			Acts       []StoredActivity
		}
		var groups []*weekGroup
		groupMap := map[string]*weekGroup{}

		for _, act := range acts {
			actTime := parseActivityTime(act.StartDateLocal, act.CreatedAt)
			y, wNum := actTime.ISOWeek()
			key := fmt.Sprintf("%d-W%02d", y, wNum)
			g, exists := groupMap[key]
			if !exists {
				g = &weekGroup{Key: key, LatestTime: actTime}
				groupMap[key] = g
				groups = append(groups, g)
			} else if actTime.After(g.LatestTime) {
				g.LatestTime = actTime
			}
			g.Acts = append(g.Acts, act)
		}

		for _, g := range groups {
			var desc strings.Builder
			fmt.Fprintf(&desc, "<p><strong>%s's workouts for week %s (%d activities):</strong></p><ul>",
				template.HTMLEscapeString(a.cfg.AthleteName), g.Key, len(g.Acts))
			for _, act := range g.Acts {
				kURL := a.kudosURL(act.ID, sub.ID)
				stats := formatStatsLine(act)
				fmt.Fprintf(&desc, `<li><strong>%s</strong> (%s) — %s `,
					template.HTMLEscapeString(act.Name),
					template.HTMLEscapeString(act.SportType),
					template.HTMLEscapeString(formatPrettyDate(act.StartDateLocal)))
				if stats != "" {
					fmt.Fprintf(&desc, `[%s] `, template.HTMLEscapeString(stats))
				}
				fmt.Fprintf(&desc, `— <a href="%s">👍 Give Kudos</a></li>`, template.HTMLEscapeString(kURL))
			}
			desc.WriteString("</ul>")
			items = append(items, rssItem{
				Title:       fmt.Sprintf("%s's Weekly Workout Digest (%s — %d activities)", a.cfg.AthleteName, g.Key, len(g.Acts)),
				Link:        a.prefURL(sub.ID),
				GUID:        "digest-" + g.Key + "-" + sub.ID,
				PubDate:     g.LatestTime.Format(time.RFC1123Z),
				Description: desc.String(),
			})
		}
	}

	feedTitle := fmt.Sprintf("%s's Workouts (Per-Activity Feed for %s)", a.cfg.AthleteName, sub.Name)
	if mode == "weekly" {
		feedTitle = fmt.Sprintf("%s's Workouts (Weekly Digest Feed for %s)", a.cfg.AthleteName, sub.Name)
	}

	out := rssRoot{
		Version: "2.0",
		Channel: rssChannel{
			Title:       feedTitle,
			Link:        a.prefURL(sub.ID),
			Description: fmt.Sprintf("Personal workout feed & 1-click kudos links for %s", sub.Name),
			Items:       items,
		},
	}

	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	_, _ = w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	_ = enc.Encode(out)
}

// ============================================================================
// HTTP Handlers: Bot-Safe 1-Click Kudos + ntfy Push Alert
// ============================================================================

func (a *App) handleKudosGet(w http.ResponseWriter, r *http.Request) {
	actID := r.URL.Query().Get("act")
	who := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("who")))
	sig := r.URL.Query().Get("sig")

	if actID == "" || who == "" || !a.verify("kudos", actID+":"+who, sig) {
		http.Error(w, "Invalid kudos link signature", http.StatusForbidden)
		return
	}

	act, err := a.getActivity(actID)
	if err != nil {
		http.Error(w, "Activity not found", http.StatusNotFound)
		return
	}
	sub, err := a.getSubscriber(who)
	if err != nil {
		http.Error(w, "Subscriber not found", http.StatusNotFound)
		return
	}

	data := struct {
		Activity    StoredActivity
		Subscriber  Subscriber
		AthleteName string
		Sig         string
		PrefURL     string
	}{
		Activity:    act,
		Subscriber:  sub,
		AthleteName: a.cfg.AthleteName,
		Sig:         sig,
		PrefURL:     a.prefURL(sub.ID),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = kudosPageTmpl.Execute(w, data)
}

func (a *App) handleKudosPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}
	actID := r.FormValue("act")
	who := strings.ToLower(strings.TrimSpace(r.FormValue("who")))
	sig := r.FormValue("sig")

	if actID == "" || who == "" || !a.verify("kudos", actID+":"+who, sig) {
		http.Error(w, "Invalid signature", http.StatusForbidden)
		return
	}

	act, err := a.getActivity(actID)
	if err != nil {
		http.Error(w, "Activity not found", http.StatusNotFound)
		return
	}
	sub, err := a.getSubscriber(who)
	if err != nil {
		http.Error(w, "Subscriber not found", http.StatusNotFound)
		return
	}

	res, err := a.db.Exec(`
		INSERT OR IGNORE INTO kudos (activity_id, subscriber_id, subscriber_name, created_at)
		VALUES (?, ?, ?, ?)
	`, act.ID, sub.ID, sub.Name, time.Now().UTC())
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	rowsAffected, _ := res.RowsAffected()
	names := a.listKudosNames(act.ID)

	if rowsAffected > 0 {
		go a.sendNtfyNotification(act, sub, len(names))
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":          true,
		"alreadySent": rowsAffected == 0,
		"totalKudos":  len(names),
		"cheeredBy":   names,
	})
}

func (a *App) sendNtfyNotification(act StoredActivity, sub Subscriber, total int) {
	if a.cfg.NtfyTopicURL == "" {
		log.Printf("[KUDOS] %s gave kudos on %q (total: %d)", sub.Name, act.Name, total)
		return
	}
	msg := fmt.Sprintf("👍 %s gave you kudos on %q (%s)! Total kudos: %d",
		sub.Name, act.Name, act.SportType, total)
	req, err := http.NewRequest("POST", a.cfg.NtfyTopicURL, strings.NewReader(msg))
	if err != nil {
		return
	}
	req.Header.Set("Title", "New Workout Kudos!")
	req.Header.Set("Tags", "muscle,tada")
	if a.cfg.NtfyToken != "" {
		req.Header.Set("Authorization", "Bearer "+a.cfg.NtfyToken)
	}
	resp, err := a.httpClient.Do(req)
	if err == nil && resp != nil {
		resp.Body.Close()
	}
}

// ============================================================================
// Database Query Helpers & Formatting
// ============================================================================

func (a *App) getSubscriber(id string) (Subscriber, error) {
	var s Subscriber
	err := a.db.QueryRow(`SELECT id, name, email, pref FROM subscribers WHERE id = ?`, id).
		Scan(&s.ID, &s.Name, &s.Email, &s.Pref)
	return s, err
}

func (a *App) listSubscribersByPref(pref string) ([]Subscriber, error) {
	rows, err := a.db.Query(`SELECT id, name, email, pref FROM subscribers WHERE pref = ?`, pref)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Subscriber
	for rows.Next() {
		var s Subscriber
		if err := rows.Scan(&s.ID, &s.Name, &s.Email, &s.Pref); err == nil {
			out = append(out, s)
		}
	}
	return out, nil
}

func (a *App) getActivity(id string) (StoredActivity, error) {
	var act StoredActivity
	var digestInt int
	err := a.db.QueryRow(`
		SELECT id, name, sport_type, start_date_local, location_str,
		       distance_m, moving_time_s, elevation_m, image_url, digest_sent, created_at
		FROM activities WHERE id = ?
	`, id).Scan(&act.ID, &act.Name, &act.SportType, &act.StartDateLocal, &act.LocationStr,
		&act.DistanceM, &act.MovingTimeS, &act.ElevationM, &act.ImageURL, &digestInt, &act.CreatedAt)
	act.DigestSent = digestInt == 1
	return act, err
}

func (a *App) getPendingDigestActivities() ([]StoredActivity, error) {
	rows, err := a.db.Query(`
		SELECT id, name, sport_type, start_date_local, location_str,
		       distance_m, moving_time_s, elevation_m, image_url, digest_sent, created_at
		FROM activities WHERE digest_sent = 0
		ORDER BY start_date_local DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanActivities(rows)
}

func (a *App) listRecentActivities(limit int) ([]StoredActivity, error) {
	rows, err := a.db.Query(`
		SELECT id, name, sport_type, start_date_local, location_str,
		       distance_m, moving_time_s, elevation_m, image_url, digest_sent, created_at
		FROM activities
		ORDER BY start_date_local DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanActivities(rows)
}

func scanActivities(rows *sql.Rows) ([]StoredActivity, error) {
	var out []StoredActivity
	for rows.Next() {
		var act StoredActivity
		var digestInt int
		if err := rows.Scan(&act.ID, &act.Name, &act.SportType, &act.StartDateLocal, &act.LocationStr,
			&act.DistanceM, &act.MovingTimeS, &act.ElevationM, &act.ImageURL, &digestInt, &act.CreatedAt); err == nil {
			act.DigestSent = digestInt == 1
			out = append(out, act)
		}
	}
	return out, nil
}

func (a *App) listKudosNames(activityID string) []string {
	rows, err := a.db.Query(`SELECT subscriber_name FROM kudos WHERE activity_id = ? ORDER BY created_at ASC`, activityID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err == nil {
			names = append(names, n)
		}
	}
	return names
}

func formatStatsLine(act StoredActivity) string {
	var parts []string
	if act.DistanceM > 0 {
		km := act.DistanceM / 1000.0
		mi := act.DistanceM / 1609.344
		parts = append(parts, fmt.Sprintf("%.2f km (%.2f mi)", km, mi))
	}
	if act.MovingTimeS > 0 {
		h := act.MovingTimeS / 3600
		m := (act.MovingTimeS % 3600) / 60
		if h > 0 {
			parts = append(parts, fmt.Sprintf("%dh %dm", h, m))
		} else {
			parts = append(parts, fmt.Sprintf("%dm", m))
		}
	}
	if act.ElevationM > 0 {
		parts = append(parts, fmt.Sprintf("%.0f m elev", act.ElevationM))
	}
	return strings.Join(parts, " • ")
}

func parseActivityTime(startLocal string, fallback time.Time) time.Time {
	// 1. Try formats that include an explicit timezone offset first
	if t, err := time.Parse(time.RFC3339, startLocal); err == nil {
		return t
	}
	// 2. Parse local Dreeve timestamps using the container's local timezone (TZ)
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, startLocal, time.Local); err == nil {
			return t
		}
	}
	return fallback
}

func formatPrettyDate(startLocal string) string {
	t := parseActivityTime(startLocal, time.Time{})
	if t.IsZero() {
		return startLocal
	}
	return t.Format("Mon, Jan 2, 2006 at 3:04 PM")
}

// ============================================================================
// Embedded HTML Templates (Email, Kudos Page, Preferences & RSS Page)
// ============================================================================

var emailTmpl = template.Must(template.New("email").Parse(`<!DOCTYPE html>
<html>
<body style="font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; background:#f4f5f7; margin:0; padding:24px; color:#1f2937;">
  <div style="max-width:580px; margin:0 auto; background:#ffffff; border-radius:12px; padding:24px; box-shadow:0 2px 6px rgba(0,0,0,0.06);">
    <h2 style="margin-top:0; color:#111827;">
      {{if .IsWeekly}}📅 {{.AthleteName}}'s Weekly Workout Digest{{else}}🏅 {{.AthleteName}} logged a new workout!{{end}}
    </h2>
    <p style="color:#4b5563; font-size:15px;">Hi {{.SubscriberName}},</p>

    {{range .Cards}}
    <div style="border:1px solid #e5e7eb; border-radius:10px; padding:18px; margin-bottom:18px; background:#fafafa;">
      <div style="font-size:12px; font-weight:700; text-transform:uppercase; color:#ea580c; letter-spacing:0.05em;">
        {{.SportType}} • {{.FormattedDate}}
      </div>
      <h3 style="margin:6px 0 8px 0; font-size:20px; color:#111827;">{{.Name}}</h3>
      {{if .StatsLine}}
        <p style="margin:4px 0; font-size:15px; font-weight:600; color:#374151;">📊 {{.StatsLine}}</p>
      {{end}}
      {{if .LocationStr}}
        <p style="margin:4px 0 12px 0; font-size:14px; color:#6b7280;">📍 {{.LocationStr}}</p>
      {{end}}
      {{if .ImageURL}}
        <div style="margin:12px 0;">
          <img src="{{.ImageURL}}" alt="Activity photo" style="max-width:100%; border-radius:8px; display:block;" />
        </div>
      {{end}}
      <div style="margin-top:14px;">
        <a href="{{.KudosURL}}" style="display:inline-block; background:#ea580c; color:#ffffff; text-decoration:none; font-weight:600; font-size:14px; padding:10px 18px; border-radius:8px;">
          👍 Give Kudos to {{$.AthleteName}}
        </a>
      </div>
    </div>
    {{end}}

    <hr style="border:none; border-top:1px solid #e5e7eb; margin:24px 0 16px 0;" />
    <div style="font-size:12px; color:#6b7280; line-height:1.6;">
      <p style="margin:0 0 8px 0;">
        <strong>Email Preferences:</strong>
        <a href="{{.PrefURL}}" style="color:#ea580c; text-decoration:underline;">
          Unsubscribe, re-subscribe, or switch between Per-Activity &amp; Weekly Digest emails
        </a>
      </p>
      <p style="margin:0;">
        <strong>Your Personal RSS Feeds:</strong>
        <a href="{{.InstantRSSURL}}" style="color:#4b5563;">Per-Activity RSS</a> •
        <a href="{{.WeeklyRSSURL}}" style="color:#4b5563;">Weekly Digest RSS</a>
      </p>
    </div>
  </div>
</body>
</html>`))

var kudosPageTmpl = template.Must(template.New("kudos").Parse(`<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Give Kudos to {{.AthleteName}}</title>
</head>
<body style="font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif; background:#f4f5f7; margin:0; padding:24px; color:#1f2937;">
  <div style="max-width:480px; margin:40px auto; background:#fff; border-radius:12px; padding:28px; text-align:center; box-shadow:0 4px 12px rgba(0,0,0,0.08);">
    <div id="status-icon" style="font-size:48px; margin-bottom:12px;">⏳</div>
    <h2 id="status-title" style="margin:0 0 8px 0;">Sending your kudos to {{.AthleteName}}...</h2>
    <p style="color:#6b7280; margin:0 0 20px 0;">Workout: <strong>{{.Activity.Name}}</strong> ({{.Activity.SportType}})</p>
    <div id="kudos-roster" style="font-size:14px; color:#374151; background:#f9fafb; padding:12px; border-radius:8px; display:none;"></div>

    <noscript>
      <form method="POST" action="/api/kudos" style="margin-top:16px;">
        <input type="hidden" name="act" value="{{.Activity.ID}}" />
        <input type="hidden" name="who" value="{{.Subscriber.ID}}" />
        <input type="hidden" name="sig" value="{{.Sig}}" />
        <button type="submit" style="background:#ea580c; color:#fff; border:none; padding:12px 20px; border-radius:8px; font-size:16px; cursor:pointer;">
          Confirm 👍 Kudos
        </button>
      </form>
    </noscript>

    <hr style="border:none; border-top:1px solid #e5e7eb; margin:24px 0 12px 0;" />
    <p style="font-size:12px; color:#6b7280; margin:0;">
      <a href="{{.PrefURL}}" style="color:#ea580c;">Manage your email &amp; RSS preferences</a>
    </p>
  </div>

  <script>
    (async function() {
      const body = new URLSearchParams({
        act: "{{.Activity.ID}}",
        who: "{{.Subscriber.ID}}",
        sig: "{{.Sig}}"
      });
      try {
        const res = await fetch("/api/kudos", { method: "POST", body });
        const data = await res.json();
        document.getElementById("status-icon").textContent = "🎉";
        document.getElementById("status-title").textContent =
          data.alreadySent ? "You've already cheered this workout!" : "Kudos sent to {{.AthleteName}}!";
        const roster = document.getElementById("kudos-roster");
        roster.style.display = "block";
        roster.textContent = "Cheered by (" + data.totalKudos + "): " + (data.cheeredBy || []).join(", ");
      } catch (e) {
        document.getElementById("status-icon").textContent = "⚠️";
        document.getElementById("status-title").textContent = "Could not send automatically — please refresh.";
      }
    })();
  </script>
</body>
</html>`))

var prefPageTmpl = template.Must(template.New("pref").Parse(`<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>{{.AthleteName}}'s Workout Updates — Preferences</title>
</head>
<body style="font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif; background:#f4f5f7; margin:0; padding:24px; color:#1f2937;">
  <div style="max-width:540px; margin:32px auto; background:#fff; border-radius:12px; padding:28px; box-shadow:0 4px 12px rgba(0,0,0,0.08);">
    <h2 style="margin-top:0;">⚙️ Subscription Preferences for {{.Subscriber.Name}}</h2>
    <p style="color:#4b5563; font-size:14px;">
      Choose how you'd like to receive <strong>{{.AthleteName}}</strong>'s workout updates at <code>{{.Subscriber.Email}}</code>.
      You can bookmark this page or use the link in any old email to switch modes or re-subscribe anytime.
    </p>

    {{if .Saved}}
    <div style="background:#ecfdf5; border:1px solid #10b981; color:#065f46; padding:12px 16px; border-radius:8px; margin-bottom:18px; font-size:14px;">
      ✅ Your email preference has been updated to <strong>{{.Subscriber.Pref}}</strong>!
    </div>
    {{end}}

    <form method="POST" action="/preferences">
      <input type="hidden" name="who" value="{{.Subscriber.ID}}" />
      <input type="hidden" name="sig" value="{{.Sig}}" />

      <label style="display:block; padding:12px; border:1px solid #d1d5db; border-radius:8px; margin-bottom:10px; cursor:pointer;">
        <input type="radio" name="pref" value="instant" {{if eq .Subscriber.Pref "instant"}}checked{{end}} />
        <strong>Per-Activity Emails (Instant)</strong>
        <div style="font-size:13px; color:#6b7280; margin-left:22px;">Get an email with a 1-click Kudos button right after {{.AthleteName}} uploads a workout.</div>
      </label>

      <label style="display:block; padding:12px; border:1px solid #d1d5db; border-radius:8px; margin-bottom:10px; cursor:pointer;">
        <input type="radio" name="pref" value="weekly" {{if eq .Subscriber.Pref "weekly"}}checked{{end}} />
        <strong>Weekly Digest Email</strong>
        <div style="font-size:13px; color:#6b7280; margin-left:22px;">One summary email per week listing all workouts and individual Kudos buttons.</div>
      </label>

      <label style="display:block; padding:12px; border:1px solid #d1d5db; border-radius:8px; margin-bottom:16px; cursor:pointer;">
        <input type="radio" name="pref" value="none" {{if eq .Subscriber.Pref "none"}}checked{{end}} />
        <strong>Unsubscribe from Emails</strong>
        <div style="font-size:13px; color:#6b7280; margin-left:22px;">Stop all emails. Come back to this link anytime to re-subscribe, or use RSS below.</div>
      </label>

      <button type="submit" style="background:#ea580c; color:#fff; border:none; padding:12px 22px; border-radius:8px; font-weight:600; font-size:15px; cursor:pointer;">
        Save Email Preference
      </button>
    </form>

    <hr style="border:none; border-top:1px solid #e5e7eb; margin:26px 0 18px 0;" />
    <h3 style="margin:0 0 8px 0; font-size:16px;">📡 Your Personal RSS Feeds</h3>
    <p style="font-size:13px; color:#4b5563; margin:0 0 12px 0;">
      Prefer an RSS reader? These private feed URLs include your personal 1-click Kudos button inside every item:
    </p>
    <div style="font-size:13px; background:#f9fafb; padding:12px; border-radius:8px; word-break:break-all;">
      <div style="margin-bottom:10px;">
        <strong>Per-Activity RSS Feed:</strong><br/>
        <a href="{{.InstantRSSURL}}" style="color:#ea580c;">{{.InstantRSSURL}}</a>
      </div>
      <div>
        <strong>Weekly Digest RSS Feed:</strong><br/>
        <a href="{{.WeeklyRSSURL}}" style="color:#ea580c;">{{.WeeklyRSSURL}}</a>
      </div>
    </div>
  </div>
</body>
</html>`))

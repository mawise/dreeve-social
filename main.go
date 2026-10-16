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
	"image"
	"image/color"
	"image/draw"
	"image/png"
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

const (
	mapWidth       = 640
	mapHeight      = 400
	mapPadding     = 44
	tileSize       = 256
	tileCacheTTL   = 30 * 24 * time.Hour
	maxTrackPoints = 600
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
	OSMTileTemplate string
}

type App struct {
	cfg        Config
	db         *sql.DB
	httpClient *http.Client
}

// DreeveActivity matches GET /api/v1/activities items from docs.dreeve.app/api/
type DreeveActivity struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	SportType       string  `json:"sportType"`
	WorldType       string  `json:"worldType"`
	StartDateLocal  string  `json:"startDateLocal"`
	HasGPX          bool    `json:"hasGpx"`
	EncodedPolyline *string `json:"encodedPolyline"`
	Location        struct {
		CountryCode *string `json:"countryCode"`
		State       *string `json:"state"`
		City        *string `json:"city"`
	} `json:"location"`
	Images []struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	} `json:"images"`
	DistanceMeters     float64 `json:"distance"`
	MovingTimeSeconds  int     `json:"movingTime"`
	ElevationGainMeter float64 `json:"elevationGain"`
}

type StoredActivity struct {
	ID              string
	Name            string
	SportType       string
	StartDateLocal  string
	LocationStr     string
	DistanceM       float64
	MovingTimeS     int
	ElevationM      float64
	ImageURL        string // Legacy single image column
	ImageURLs       []string
	EncodedPolyline string
	DigestSent      bool
	CreatedAt       time.Time
}

type Subscriber struct {
	ID    string // URL-safe slug, e.g. "alice"
	Name  string // Display name, e.g. "Alice"
	Email string
	Pref  string // "instant", "weekly", or "none"
}

type LatLon struct {
	Lat float64
	Lon float64
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
		DBPath:          getEnv("DB_PATH", "/data/sidecar.db"),
		ListenAddr:      getEnv("LISTEN_ADDR", ":8080"),
		AthleteName:     getEnv("ATHLETE_NAME", "Alex"),
		DreeveURL:       strings.TrimRight(getEnv("DREEVE_URL", "http://dreeve:8080"), "/"),
		DreeveToken:     getEnv("DREEVE_API_TOKEN", ""),
		PublicBaseURL:   strings.TrimRight(getEnv("PUBLIC_BASE_URL", "http://localhost:8090"), "/"),
		HMACSecret:      getEnv("HMAC_SECRET", "change-me-in-production"),
		PollInterval:    time.Duration(pollMin) * time.Minute,
		DigestWeekday:   time.Weekday(digestDayInt),
		DigestHour:      digestHour,
		MailgunDomain:   getEnv("MAILGUN_DOMAIN", ""),
		MailgunAPIKey:   getEnv("MAILGUN_API_KEY", ""),
		MailgunFrom:     getEnv("MAILGUN_FROM", "Dreeve Social <noreply@example.com>"),
		MailgunAPIBase:  strings.TrimRight(getEnv("MAILGUN_API_BASE", "https://api.mailgun.net"), "/"),
		NtfyTopicURL:    getEnv("NTFY_TOPIC_URL", ""),
		NtfyToken:       getEnv("NTFY_TOKEN", ""),
		InitialFriends:  getEnv("FRIENDS", ""),
		SeedExisting:    strings.EqualFold(getEnv("SEED_EXISTING_WITHOUT_EMAIL", "true"), "true"),
		OSMTileTemplate: getEnv("OSM_TILE_URL", "https://tile.openstreetmap.org/%d/%d/%d.png"),
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
		httpClient: &http.Client{Timeout: 20 * time.Second},
	}

	if err := app.seedFriendsFromEnv(); err != nil {
		log.Printf("warning seeding friends: %v", err)
	}

	// Background goroutine: Dreeve poller + Weekly Digest scheduler + Tile cache cleanup
	go app.backgroundLoop()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /feed.xml", app.handleRSSFeed)
	mux.HandleFunc("GET /map.png", app.handleMapPNG)
	mux.HandleFunc("GET /photo", app.handleActivityPhoto)
	mux.HandleFunc("GET /kudos", app.handleKudosGet)
	mux.HandleFunc("POST /api/kudos", app.handleKudosPost)
	mux.HandleFunc("GET /preferences", app.handlePreferencesGet)
	mux.HandleFunc("POST /preferences", app.handlePreferencesPost)

	log.Printf("dreeve-social listening on %s (public URL: %s)", cfg.ListenAddr, cfg.PublicBaseURL)
	log.Fatal(http.ListenAndServe(cfg.ListenAddr, mux))
}

// ============================================================================
// Database Initialization & Migrations
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
		image_urls_json TEXT NOT NULL DEFAULT '[]',
		encoded_polyline TEXT NOT NULL DEFAULT '',
		digest_sent INTEGER NOT NULL DEFAULT 0,
		created_at DATETIME NOT NULL
	);

	CREATE TABLE IF NOT EXISTS tile_cache (
		z INTEGER NOT NULL,
		x INTEGER NOT NULL,
		y INTEGER NOT NULL,
		png_data BLOB NOT NULL,
		fetched_at DATETIME NOT NULL,
		PRIMARY KEY (z, x, y)
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
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}

	// Safe migrations for existing databases created before map & multi-photo support
	_, _ = db.Exec(`ALTER TABLE activities ADD COLUMN image_urls_json TEXT NOT NULL DEFAULT '[]'`)
	_, _ = db.Exec(`ALTER TABLE activities ADD COLUMN encoded_polyline TEXT NOT NULL DEFAULT ''`)

	return db, nil
}

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

func (a *App) mapURL(activityID string) string {
	sig := a.sign("map", activityID)
	q := url.Values{}
	q.Set("act", activityID)
	q.Set("sig", sig)
	return a.cfg.PublicBaseURL + "/map.png?" + q.Encode()
}

func (a *App) photoURL(activityID string, idx int) string {
	payload := fmt.Sprintf("%s:%d", activityID, idx)
	sig := a.sign("photo", payload)
	q := url.Values{}
	q.Set("act", activityID)
	q.Set("idx", strconv.Itoa(idx))
	q.Set("sig", sig)
	return a.cfg.PublicBaseURL + "/photo?" + q.Encode()
}

// ============================================================================
// Dreeve Polling, GPX Track Parsing & Weekly Digest Loop
// ============================================================================

func (a *App) backgroundLoop() {
	a.pruneExpiredTiles()
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
			a.pruneExpiredTiles()
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

	isFirstSync := a.isFirstSync()

	for i := len(activities) - 1; i >= 0; i-- {
		act := activities[i]
		if act.ID == "" || a.activityExists(act.ID) {
			continue
		}

		poly := ""
		if act.EncodedPolyline != nil {
			poly = strings.TrimSpace(*act.EncodedPolyline)
		}

		// Fetch GPX if we need stats OR if encodedPolyline wasn't included in the summary
		if act.HasGPX && (act.DistanceMeters == 0 || poly == "") {
			distM, elevM, durS, gpxPoly := a.computeStatsAndPolylineFromGPX(act.ID)
			if distM > 0 && act.DistanceMeters == 0 {
				act.DistanceMeters = distM
			}
			if elevM > 0 && act.ElevationGainMeter == 0 {
				act.ElevationGainMeter = elevM
			}
			if durS > 0 && act.MovingTimeSeconds == 0 {
				act.MovingTimeSeconds = durS
			}
			if poly == "" && gpxPoly != "" {
				poly = gpxPoly
			}
		}

		stored := toStoredActivity(act, poly)
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

		// Pre-warm OSM tile cache for the new workout before sending emails
		if stored.EncodedPolyline != "" {
			_, _ = a.renderMapForPolyline(stored.EncodedPolyline)
		}

		log.Printf("new activity detected: %s (%s)", stored.Name, stored.ID)
		a.sendInstantEmails(stored)
	}

	if isFirstSync {
		_, _ = a.db.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('initial_sync_done', '1')`)
	}
}

func parseDreeveActivities(raw []byte) ([]DreeveActivity, error) {
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

func (a *App) computeStatsAndPolylineFromGPX(activityID string) (distMeters float64, elevGain float64, durationSec int, encodedPoly string) {
	req, err := http.NewRequest("GET", a.cfg.DreeveURL+"/api/v1/activities/"+url.PathEscape(activityID)+"/gpx", nil)
	if err != nil {
		return 0, 0, 0, ""
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.DreeveToken)
	resp, err := a.httpClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return 0, 0, 0, ""
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
		return 0, 0, 0, ""
	}

	var prevLat, prevLon, prevEle float64
	var havePrev bool
	var firstTime, lastTime time.Time
	var pts []LatLon

	for _, trk := range gpx.Trk {
		for _, seg := range trk.TrkSeg {
			for _, pt := range seg.TrkPt {
				pts = append(pts, LatLon{Lat: pt.Lat, Lon: pt.Lon})
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
	if len(pts) > 1 {
		encodedPoly = encodePolyline(downsamplePoints(pts, maxTrackPoints))
	}
	return distMeters, elevGain, durationSec, encodedPoly
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

func toStoredActivity(act DreeveActivity, poly string) StoredActivity {
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
	var imgURLs []string
	for _, im := range act.Images {
		if strings.TrimSpace(im.URL) != "" {
			imgURLs = append(imgURLs, strings.TrimSpace(im.URL))
		}
	}
	firstImg := ""
	if len(imgURLs) > 0 {
		firstImg = imgURLs[0]
	}
	return StoredActivity{
		ID:              act.ID,
		Name:            act.Name,
		SportType:       act.SportType,
		StartDateLocal:  act.StartDateLocal,
		LocationStr:     strings.Join(locParts, ", "),
		DistanceM:       act.DistanceMeters,
		MovingTimeS:     act.MovingTimeSeconds,
		ElevationM:      act.ElevationGainMeter,
		ImageURL:        firstImg,
		ImageURLs:       imgURLs,
		EncodedPolyline: poly,
		CreatedAt:       time.Now().UTC(),
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
	imgsJSON, _ := json.Marshal(act.ImageURLs)
	_, err := a.db.Exec(`
		INSERT INTO activities (
			id, name, sport_type, start_date_local, location_str,
			distance_m, moving_time_s, elevation_m, image_url, image_urls_json,
			encoded_polyline, digest_sent, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, act.ID, act.Name, act.SportType, act.StartDateLocal, act.LocationStr,
		act.DistanceM, act.MovingTimeS, act.ElevationM, act.ImageURL, string(imgsJSON),
		act.EncodedPolyline, digestInt, act.CreatedAt)
	return err
}

// ============================================================================
// OSM Tile Caching (30 Days), Map Rendering & Photo Proxy
// ============================================================================

func (a *App) pruneExpiredTiles() {
	cutoff := time.Now().UTC().Add(-tileCacheTTL)
	_, _ = a.db.Exec(`DELETE FROM tile_cache WHERE fetched_at < ?`, cutoff)
}

func (a *App) getOSMTile(z, x, y int) (image.Image, error) {
	var pngBytes []byte
	var fetchedAt time.Time
	err := a.db.QueryRow(`SELECT png_data, fetched_at FROM tile_cache WHERE z = ? AND x = ? AND y = ?`, z, x, y).
		Scan(&pngBytes, &fetchedAt)
	if err == nil && time.Since(fetchedAt) < tileCacheTTL {
		if img, decErr := png.Decode(bytes.NewReader(pngBytes)); decErr == nil {
			return img, nil
		}
	}

	n := 1 << z
	wrapX := ((x % n) + n) % n
	tileURL := fmt.Sprintf(a.cfg.OSMTileTemplate, z, wrapX, y)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", tileURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", fmt.Sprintf("dreeve-social/1.0 (+%s)", a.cfg.PublicBaseURL))

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OSM tile HTTP %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return nil, err
	}

	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}

	_, _ = a.db.Exec(`
		INSERT INTO tile_cache (z, x, y, png_data, fetched_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(z, x, y) DO UPDATE SET
			png_data = excluded.png_data,
			fetched_at = excluded.fetched_at
	`, z, wrapX, y, raw, time.Now().UTC())

	return img, nil
}

// renderMapForPolyline draws the GPS track onto cached OSM tiles, falling back cleanly
// to a solid white background if any OSM tile fails to load.
func (a *App) renderMapForPolyline(encoded string) ([]byte, error) {
	pts := decodePolyline(encoded)
	if len(pts) < 2 {
		return nil, fmt.Errorf("not enough track points")
	}

	minLat, maxLat := pts[0].Lat, pts[0].Lat
	minLon, maxLon := pts[0].Lon, pts[0].Lon
	for _, p := range pts[1:] {
		if p.Lat < minLat {
			minLat = p.Lat
		}
		if p.Lat > maxLat {
			maxLat = p.Lat
		}
		if p.Lon < minLon {
			minLon = p.Lon
		}
		if p.Lon > maxLon {
			maxLon = p.Lon
		}
	}

	// Pick highest zoom (16 down to 1) that fits the track within the padded canvas
	zoom := 14
	for z := 16; z >= 1; z-- {
		x1, y1 := lonLatToWorldPx(minLon, maxLat, z)
		x2, y2 := lonLatToWorldPx(maxLon, minLat, z)
		if math.Abs(x2-x1) <= float64(mapWidth-2*mapPadding) &&
			math.Abs(y2-y1) <= float64(mapHeight-2*mapPadding) {
			zoom = z
			break
		}
	}

	xMinPx, yMinPx := lonLatToWorldPx(minLon, maxLat, zoom)
	xMaxPx, yMaxPx := lonLatToWorldPx(maxLon, minLat, zoom)
	centerX := (xMinPx + xMaxPx) / 2.0
	centerY := (yMinPx + yMaxPx) / 2.0
	originX := centerX - float64(mapWidth)/2.0
	originY := centerY - float64(mapHeight)/2.0

	// 1. Start with a solid white canvas (serves as the fallback if tiles fail)
	canvas := image.NewRGBA(image.Rect(0, 0, mapWidth, mapHeight))
	draw.Draw(canvas, canvas.Bounds(), &image.Uniform{C: color.RGBA{255, 255, 255, 255}}, image.Point{}, draw.Src)

	// 2. Fetch all required OSM tiles; only composite if all tiles succeed
	minTileX := int(math.Floor(originX / tileSize))
	maxTileX := int(math.Floor((originX + float64(mapWidth) - 1) / tileSize))
	minTileY := int(math.Floor(originY / tileSize))
	maxTileY := int(math.Floor((originY + float64(mapHeight) - 1) / tileSize))
	maxValidTileY := (1 << zoom) - 1

	type placedTile struct {
		img        image.Image
		dstX, dstY int
	}
	var tiles []placedTile
	tilesOK := true

	for tx := minTileX; tx <= maxTileX && tilesOK; tx++ {
		for ty := minTileY; ty <= maxTileY; ty++ {
			if ty < 0 || ty > maxValidTileY {
				continue
			}
			tImg, err := a.getOSMTile(zoom, tx, ty)
			if err != nil {
				log.Printf("OSM tile (%d/%d/%d) error (%v); falling back to white background", zoom, tx, ty, err)
				tilesOK = false
				break
			}
			tiles = append(tiles, placedTile{
				img:  tImg,
				dstX: tx*tileSize - int(math.Round(originX)),
				dstY: ty*tileSize - int(math.Round(originY)),
			})
		}
	}

	if tilesOK {
		for _, pt := range tiles {
			r := image.Rect(pt.dstX, pt.dstY, pt.dstX+tileSize, pt.dstY+tileSize)
			draw.Draw(canvas, r, pt.img, image.Point{}, draw.Over)
		}
	}

	// 3. Convert GPS coordinates to canvas pixels
	type pxPt struct{ x, y float64 }
	pxPts := make([]pxPt, len(pts))
	for i, p := range pts {
		wx, wy := lonLatToWorldPx(p.Lon, p.Lat, zoom)
		pxPts[i] = pxPt{x: wx - originX, y: wy - originY}
	}

	// 4. Draw white casing halo first, then orange GPS track (#ea580c)
	whiteHalo := color.RGBA{255, 255, 255, 235}
	routeOrange := color.RGBA{234, 88, 12, 255}
	for i := 1; i < len(pxPts); i++ {
		drawThickSegment(canvas, pxPts[i-1].x, pxPts[i-1].y, pxPts[i].x, pxPts[i].y, 4.0, whiteHalo)
	}
	for i := 1; i < len(pxPts); i++ {
		drawThickSegment(canvas, pxPts[i-1].x, pxPts[i-1].y, pxPts[i].x, pxPts[i].y, 2.5, routeOrange)
	}

	// 5. Draw Start (green) and End (red) markers
	startPt := pxPts[0]
	endPt := pxPts[len(pxPts)-1]
	drawFilledCircle(canvas, startPt.x, startPt.y, 6.5, color.RGBA{255, 255, 255, 255})
	drawFilledCircle(canvas, startPt.x, startPt.y, 4.5, color.RGBA{22, 163, 74, 255})
	drawFilledCircle(canvas, endPt.x, endPt.y, 6.5, color.RGBA{255, 255, 255, 255})
	drawFilledCircle(canvas, endPt.x, endPt.y, 4.5, color.RGBA{220, 38, 38, 255})

	var out bytes.Buffer
	if err := png.Encode(&out, canvas); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func drawThickSegment(img *image.RGBA, x0, y0, x1, y1, radius float64, c color.RGBA) {
	dx := x1 - x0
	dy := y1 - y0
	dist := math.Hypot(dx, dy)
	steps := int(math.Ceil(dist * 2.0))
	if steps < 1 {
		steps = 1
	}
	for s := 0; s <= steps; s++ {
		t := float64(s) / float64(steps)
		drawFilledCircle(img, x0+dx*t, y0+dy*t, radius, c)
	}
}

func drawFilledCircle(img *image.RGBA, cx, cy, radius float64, c color.RGBA) {
	minX := int(math.Floor(cx - radius - 1))
	maxX := int(math.Ceil(cx + radius + 1))
	minY := int(math.Floor(cy - radius - 1))
	maxY := int(math.Ceil(cy + radius + 1))
	bounds := img.Bounds()
	rSq := radius * radius

	for y := minY; y <= maxY; y++ {
		if y < bounds.Min.Y || y >= bounds.Max.Y {
			continue
		}
		for x := minX; x <= maxX; x++ {
			if x < bounds.Min.X || x >= bounds.Max.X {
				continue
			}
			dx := float64(x) + 0.5 - cx
			dy := float64(y) + 0.5 - cy
			if dx*dx+dy*dy <= rSq {
				img.SetRGBA(x, y, c)
			}
		}
	}
}

func lonLatToWorldPx(lon, lat float64, zoom int) (float64, float64) {
	if lat > 85.0511 {
		lat = 85.0511
	} else if lat < -85.0511 {
		lat = -85.0511
	}
	scale := float64(int(tileSize) << zoom)
	x := (lon + 180.0) / 360.0 * scale
	latRad := lat * math.Pi / 180.0
	y := (1.0 - math.Log(math.Tan(latRad)+1.0/math.Cos(latRad))/math.Pi) / 2.0 * scale
	return x, y
}

func downsamplePoints(pts []LatLon, maxPts int) []LatLon {
	if len(pts) <= maxPts || maxPts < 2 {
		return pts
	}
	out := make([]LatLon, 0, maxPts)
	step := float64(len(pts)-1) / float64(maxPts-1)
	for i := 0; i < maxPts-1; i++ {
		out = append(out, pts[int(math.Round(float64(i)*step))])
	}
	out = append(out, pts[len(pts)-1])
	return out
}

func encodePolyline(pts []LatLon) string {
	var buf strings.Builder
	var prevLat, prevLon int
	for _, p := range pts {
		lat := int(math.Round(p.Lat * 1e5))
		lon := int(math.Round(p.Lon * 1e5))
		encodeSignedNumber(&buf, lat-prevLat)
		encodeSignedNumber(&buf, lon-prevLon)
		prevLat, prevLon = lat, lon
	}
	return buf.String()
}

func encodeSignedNumber(buf *strings.Builder, num int) {
	v := num << 1
	if num < 0 {
		v = ^v
	}
	for v >= 0x20 {
		buf.WriteByte(byte((0x20 | (v & 0x1f)) + 63))
		v >>= 5
	}
	buf.WriteByte(byte(v + 63))
}

func decodePolyline(encoded string) []LatLon {
	var pts []LatLon
	var index, lat, lon int
	for index < len(encoded) {
		var result, shift int
		for {
			if index >= len(encoded) {
				return pts
			}
			b := int(encoded[index]) - 63
			index++
			result |= (b & 0x1f) << shift
			shift += 5
			if b < 0x20 {
				break
			}
		}
		if (result & 1) != 0 {
			lat += ^(result >> 1)
		} else {
			lat += result >> 1
		}

		result, shift = 0, 0
		for {
			if index >= len(encoded) {
				return pts
			}
			b := int(encoded[index]) - 63
			index++
			result |= (b & 0x1f) << shift
			shift += 5
			if b < 0x20 {
				break
			}
		}
		if (result & 1) != 0 {
			lon += ^(result >> 1)
		} else {
			lon += result >> 1
		}

		pts = append(pts, LatLon{Lat: float64(lat) / 1e5, Lon: float64(lon) / 1e5})
	}
	return pts
}

// handleMapPNG serves the rendered GPS route map PNG for an activity.
func (a *App) handleMapPNG(w http.ResponseWriter, r *http.Request) {
	actID := r.URL.Query().Get("act")
	sig := r.URL.Query().Get("sig")
	if actID == "" || !a.verify("map", actID, sig) {
		http.Error(w, "Invalid map signature", http.StatusForbidden)
		return
	}

	act, err := a.getActivity(actID)
	if err != nil || act.EncodedPolyline == "" {
		http.Error(w, "Map not found for activity", http.StatusNotFound)
		return
	}

	pngBytes, err := a.renderMapForPolyline(act.EncodedPolyline)
	if err != nil {
		http.Error(w, "Failed to render map", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(pngBytes)
}

// handleActivityPhoto proxies Dreeve user photos so friends can view them in Email/RSS
// even when Dreeve is hosted on a private internal Docker network.
func (a *App) handleActivityPhoto(w http.ResponseWriter, r *http.Request) {
	actID := r.URL.Query().Get("act")
	idxStr := r.URL.Query().Get("idx")
	sig := r.URL.Query().Get("sig")

	idx, err := strconv.Atoi(idxStr)
	if err != nil || idx < 0 || actID == "" || !a.verify("photo", fmt.Sprintf("%s:%d", actID, idx), sig) {
		http.Error(w, "Invalid photo signature", http.StatusForbidden)
		return
	}

	act, err := a.getActivity(actID)
	if err != nil || idx >= len(act.ImageURLs) {
		http.Error(w, "Photo not found", http.StatusNotFound)
		return
	}

	rawURL := act.ImageURLs[idx]
	targetURL := rawURL
	if strings.HasPrefix(rawURL, "/") {
		targetURL = a.cfg.DreeveURL + rawURL
	}

	req, err := http.NewRequestWithContext(r.Context(), "GET", targetURL, nil)
	if err != nil {
		http.Error(w, "Bad photo URL", http.StatusBadGateway)
		return
	}
	if strings.HasPrefix(targetURL, a.cfg.DreeveURL) && a.cfg.DreeveToken != "" {
		req.Header.Set("Authorization", "Bearer "+a.cfg.DreeveToken)
	}

	resp, err := a.httpClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		http.Error(w, "Upstream photo unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "image/jpeg")
	}
	w.Header().Set("Cache-Control", "public, max-age=604800")
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 15*1024*1024))
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
	MapImageURL   string
	PhotoURLs     []string
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

		mapImg := ""
		if act.EncodedPolyline != "" {
			mapImg = a.mapURL(act.ID)
		}
		var photos []string
		for idx := range act.ImageURLs {
			photos = append(photos, a.photoURL(act.ID, idx))
		}

		cards = append(cards, emailActivityCard{
			StoredActivity: act,
			FormattedDate:  fDate,
			StatsLine:      stats,
			MapImageURL:    mapImg,
			PhotoURLs:      photos,
			KudosURL:       kURL,
		})

		fmt.Fprintf(&textBuilder, "- %s (%s) on %s\n", act.Name, act.SportType, fDate)
		if stats != "" {
			fmt.Fprintf(&textBuilder, "  Stats: %s\n", stats)
		}
		if act.LocationStr != "" {
			fmt.Fprintf(&textBuilder, "  Location: %s\n", act.LocationStr)
		}
		if mapImg != "" {
			fmt.Fprintf(&textBuilder, "  Route Map: %s\n", mapImg)
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

// handleRSSFeed serves user-specific RSS feeds in either 'instant' (per-activity) or 'weekly' (digest) mode,
// including route map PNGs and all Dreeve user photos.
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
			var desc strings.Builder

			fmt.Fprintf(&desc, "<p><strong>%s</strong> (%s) — %s</p>",
				template.HTMLEscapeString(act.Name),
				template.HTMLEscapeString(act.SportType),
				template.HTMLEscapeString(formatPrettyDate(act.StartDateLocal)))
			if stats := formatStatsLine(act); stats != "" {
				fmt.Fprintf(&desc, "<p>📊 %s</p>", template.HTMLEscapeString(stats))
			}
			if act.LocationStr != "" {
				fmt.Fprintf(&desc, "<p>📍 %s</p>", template.HTMLEscapeString(act.LocationStr))
			}
			if act.EncodedPolyline != "" {
				mURL := a.mapURL(act.ID)
				fmt.Fprintf(&desc, `<p><img src="%s" alt="GPS route map" style="max-width:100%%;border-radius:8px;border:1px solid #e5e7eb;" /></p>`,
					template.HTMLEscapeString(mURL))
			}
			for idx := range act.ImageURLs {
				pURL := a.photoURL(act.ID, idx)
				fmt.Fprintf(&desc, `<p><img src="%s" alt="Workout photo" style="max-width:100%%;border-radius:8px;" /></p>`,
					template.HTMLEscapeString(pURL))
			}
			fmt.Fprintf(&desc, `<p><a href="%s"><strong>👍 Give Kudos to %s (1-Click)</strong></a></p>`,
				template.HTMLEscapeString(kURL), template.HTMLEscapeString(a.cfg.AthleteName))

			items = append(items, rssItem{
				Title:       fmt.Sprintf("%s: %s", act.SportType, act.Name),
				Link:        kURL,
				GUID:        act.ID + "-" + sub.ID,
				PubDate:     actTime.Format(time.RFC1123Z),
				Description: desc.String(),
			})
		}
	} else {
		// Group activities by ISO week and use the configured digest publish time as PubDate
		type weekGroup struct {
			Key         string
			PublishTime time.Time
			Acts        []StoredActivity
		}
		var groups []*weekGroup
		groupMap := map[string]*weekGroup{}

		for _, act := range acts {
			actTime := parseActivityTime(act.StartDateLocal, act.CreatedAt)
			y, wNum := actTime.ISOWeek()
			key := fmt.Sprintf("%d-W%02d", y, wNum)
			g, exists := groupMap[key]
			if !exists {
				g = &weekGroup{
					Key:         key,
					PublishTime: a.digestPublishTime(actTime),
				}
				groupMap[key] = g
				groups = append(groups, g)
			}
			g.Acts = append(g.Acts, act)
		}

		for _, g := range groups {
			var desc strings.Builder
			fmt.Fprintf(&desc, "<p><strong>%s's workouts for week %s (%d activities):</strong></p>",
				template.HTMLEscapeString(a.cfg.AthleteName), g.Key, len(g.Acts))
			for _, act := range g.Acts {
				kURL := a.kudosURL(act.ID, sub.ID)
				stats := formatStatsLine(act)
				fmt.Fprintf(&desc, `<div style="margin-bottom:20px;padding-bottom:16px;border-bottom:1px solid #e5e7eb;">`)
				fmt.Fprintf(&desc, `<p><strong>%s</strong> (%s) — %s`,
					template.HTMLEscapeString(act.Name),
					template.HTMLEscapeString(act.SportType),
					template.HTMLEscapeString(formatPrettyDate(act.StartDateLocal)))
				if stats != "" {
					fmt.Fprintf(&desc, `<br/>📊 %s`, template.HTMLEscapeString(stats))
				}
				if act.LocationStr != "" {
					fmt.Fprintf(&desc, `<br/>📍 %s`, template.HTMLEscapeString(act.LocationStr))
				}
				fmt.Fprintf(&desc, `</p>`)
				if act.EncodedPolyline != "" {
					mURL := a.mapURL(act.ID)
					fmt.Fprintf(&desc, `<p><img src="%s" alt="GPS route map" style="max-width:100%%;border-radius:8px;border:1px solid #e5e7eb;" /></p>`,
						template.HTMLEscapeString(mURL))
				}
				for idx := range act.ImageURLs {
					pURL := a.photoURL(act.ID, idx)
					fmt.Fprintf(&desc, `<p><img src="%s" alt="Workout photo" style="max-width:100%%;border-radius:8px;" /></p>`,
						template.HTMLEscapeString(pURL))
				}
				fmt.Fprintf(&desc, `<p><a href="%s"><strong>👍 Give Kudos</strong></a></p></div>`,
					template.HTMLEscapeString(kURL))
			}
			items = append(items, rssItem{
				Title:       fmt.Sprintf("%s's Weekly Workout Digest (%s — %d activities)", a.cfg.AthleteName, g.Key, len(g.Acts)),
				Link:        a.prefURL(sub.ID),
				GUID:        "digest-" + g.Key + "-" + sub.ID,
				PubDate:     g.PublishTime.Format(time.RFC1123Z),
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
	var imgsJSON string
	var digestInt int
	err := a.db.QueryRow(`
		SELECT id, name, sport_type, start_date_local, location_str,
		       distance_m, moving_time_s, elevation_m, image_url, image_urls_json,
		       encoded_polyline, digest_sent, created_at
		FROM activities WHERE id = ?
	`, id).Scan(&act.ID, &act.Name, &act.SportType, &act.StartDateLocal, &act.LocationStr,
		&act.DistanceM, &act.MovingTimeS, &act.ElevationM, &act.ImageURL, &imgsJSON,
		&act.EncodedPolyline, &digestInt, &act.CreatedAt)
	if err != nil {
		return act, err
	}
	act.DigestSent = digestInt == 1
	act.ImageURLs = parseImageURLs(imgsJSON, act.ImageURL)
	return act, nil
}

func (a *App) getPendingDigestActivities() ([]StoredActivity, error) {
	rows, err := a.db.Query(`
		SELECT id, name, sport_type, start_date_local, location_str,
		       distance_m, moving_time_s, elevation_m, image_url, image_urls_json,
		       encoded_polyline, digest_sent, created_at
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
		       distance_m, moving_time_s, elevation_m, image_url, image_urls_json,
		       encoded_polyline, digest_sent, created_at
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
		var imgsJSON string
		var digestInt int
		if err := rows.Scan(&act.ID, &act.Name, &act.SportType, &act.StartDateLocal, &act.LocationStr,
			&act.DistanceM, &act.MovingTimeS, &act.ElevationM, &act.ImageURL, &imgsJSON,
			&act.EncodedPolyline, &digestInt, &act.CreatedAt); err == nil {
			act.DigestSent = digestInt == 1
			act.ImageURLs = parseImageURLs(imgsJSON, act.ImageURL)
			out = append(out, act)
		}
	}
	return out, nil
}

func parseImageURLs(imgsJSON, legacyURL string) []string {
	var urls []string
	if imgsJSON != "" {
		_ = json.Unmarshal([]byte(imgsJSON), &urls)
	}
	if len(urls) == 0 && legacyURL != "" {
		urls = []string{legacyURL}
	}
	return urls
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
	if t, err := time.Parse(time.RFC3339, startLocal); err == nil {
		return t
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, startLocal, time.Local); err == nil {
			return t
		}
	}
	return fallback
}

func (a *App) digestPublishTime(t time.Time) time.Time {
	local := t.In(time.Local)
	dayOffset := isoWeekday(a.cfg.DigestWeekday) - isoWeekday(local.Weekday())
	targetDate := local.AddDate(0, 0, dayOffset)
	return time.Date(
		targetDate.Year(), targetDate.Month(), targetDate.Day(),
		a.cfg.DigestHour, 0, 0, 0, time.Local,
	)
}

func isoWeekday(w time.Weekday) int {
	if w == time.Sunday {
		return 7
	}
	return int(w)
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
      {{if .MapImageURL}}
        <div style="margin:12px 0;">
          <img src="{{.MapImageURL}}" alt="GPS Route Map" style="max-width:100%; border-radius:8px; border:1px solid #e5e7eb; display:block;" />
        </div>
      {{end}}
      {{range .PhotoURLs}}
        <div style="margin:12px 0;">
          <img src="{{.}}" alt="Activity photo" style="max-width:100%; border-radius:8px; display:block;" />
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

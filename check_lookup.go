// Package main implements a web server that serves a form for entering a
// YouTube video URL and, on submit, resolves it to the video's channel
// link, tracks live status in a SQLite database, and exposes endpoints to
// list/recheck/delete tracked channels.
//
// This is a Go port of channel_lookup.py. Config defaults are read from a
// .env file (HOST, PORT, DB_FILE, CHECK_INTERVAL_MINUTES), overridable by
// real environment variables.
//
// Run with:
//
//	go run check_lookup.go
//
// Then open http://localhost:8000 in a browser.
package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

var (
	host          string
	port          string
	dbPath        string
	checkInterval time.Duration
	db            *sql.DB
)

// loadDotEnv is a minimal .env loader (no third-party dependency). It only
// sets variables that aren't already present in the environment.
func loadDotEnv() {
	exeDir, err := os.Getwd()
	if err != nil {
		return
	}
	envPath := filepath.Join(exeDir, ".env")
	f, err := os.Open(envPath)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, value)
		}
	}
}

func getEnv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func init() {
	loadDotEnv()
	host = getEnv("HOST", "0.0.0.0")
	port = getEnv("PORT", "8000")
	dbFile := getEnv("DB_FILE", "channels.db")
	wd, _ := os.Getwd()
	dbPath = filepath.Join(wd, dbFile)
	minutes, err := strconv.Atoi(getEnv("CHECK_INTERVAL_MINUTES", "5"))
	if err != nil {
		minutes = 5
	}
	checkInterval = time.Duration(minutes) * time.Minute
}

const formPageTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>YouTube Video -> Channel Link</title>
<style>
  body { font-family: sans-serif; max-width: 640px; margin: 40px auto; }
  input[type=text] { width: 100%%; padding: 8px; font-size: 1rem; }
  button { padding: 8px 16px; font-size: 1rem; margin-top: 10px; }
  .result { margin-top: 20px; padding: 12px; border: 1px solid #ccc; }
  .error { color: #b00020; }
</style>
</head>
<body>
  <h1>YouTube Video -&gt; Channel Link</h1>
  <p><a href="/show">View saved channel statuses &rarr;</a></p>
  <form method="POST" action="/">
    <label for="url">YouTube video URL</label><br>
    <input type="text" id="url" name="url" placeholder="https://www.youtube.com/watch?v=..." value="%s" required>
    <br>
    <button type="submit">Submit</button>
  </form>
  %s
</body>
</html>
`

const showPage = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Channel Live Status</title>
<style>
  body { font-family: sans-serif; max-width: 900px; margin: 40px auto; }
  table { width: 100%; border-collapse: collapse; }
  th, td { border: 1px solid #ccc; padding: 8px; text-align: left; }
  th { background: #f4f4f4; }
  .live { color: #0a0; font-weight: bold; }
  button.delete-btn { color: #b00020; cursor: pointer; }
  #updated { color: #666; font-size: 0.9rem; margin-top: 10px; }
</style>
</head>
<body>
  <h1>Channel Live Status</h1>
  <p><a href="/">&larr; Check another video</a></p>
  <table id="status-table">
    <thead>
      <tr><th>Channel</th><th>Status</th><th>Live Feed</th><th>Last Checked</th><th></th></tr>
    </thead>
    <tbody id="status-body"></tbody>
  </table>
  <p id="updated"></p>

<script>
async function loadStatus() {
  const res = await fetch('/show_status');
  const channels = await res.json();
  const body = document.getElementById('status-body');
  body.innerHTML = '';
  for (const ch of channels) {
    const tr = document.createElement('tr');
    const statusText = ch.is_live ? '\u{1F534} Live' : 'Not live';
    const statusClass = ch.is_live ? 'live' : '';
    const liveFeedCell = ch.is_live && ch.live_video_url
      ? '<a href="' + ch.live_video_url + '" target="_blank" rel="noopener">Watch live</a>'
      : '';
    tr.innerHTML =
      '<td><a href="' + ch.url + '" target="_blank" rel="noopener">' + ch.name + '</a></td>' +
      '<td class="' + statusClass + '">' + statusText + '</td>' +
      '<td>' + liveFeedCell + '</td>' +
      '<td>' + ch.checked_at + '</td>' +
      '<td><button class="delete-btn" data-url="' + ch.url + '">Delete</button></td>';
    body.appendChild(tr);
  }
  document.getElementById('updated').textContent =
    'Updated: ' + new Date().toLocaleString();
}

async function deleteChannel(url) {
  await fetch('/channel?url=' + encodeURIComponent(url), { method: 'DELETE' });
  await loadStatus();
}

document.getElementById('status-body').addEventListener('click', (event) => {
  const btn = event.target.closest('.delete-btn');
  if (btn) {
    deleteChannel(btn.dataset.url);
  }
});

async function checkAllThenRefresh() {
  await fetch('/check_all');
  await loadStatus();
}

loadStatus();
setInterval(checkAllThenRefresh, 10 * 60 * 1000);
</script>
</body>
</html>
`

func initDB() error {
	database, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return err
	}
	db = database

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS channels (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			url TEXT NOT NULL UNIQUE,
			is_live INTEGER NOT NULL,
			checked_at TEXT NOT NULL,
			live_video_id TEXT,
			live_video_title TEXT
		)
	`)
	if err != nil {
		return err
	}

	// Migrate older databases created before the live-video columns existed.
	existingCols := map[string]bool{}
	rows, err := db.Query("PRAGMA table_info(channels)")
	if err != nil {
		return err
	}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt interface{}
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		existingCols[name] = true
	}
	rows.Close()

	if !existingCols["live_video_id"] {
		if _, err := db.Exec("ALTER TABLE channels ADD COLUMN live_video_id TEXT"); err != nil {
			return err
		}
	}
	if !existingCols["live_video_title"] {
		if _, err := db.Exec("ALTER TABLE channels ADD COLUMN live_video_title TEXT"); err != nil {
			return err
		}
	}
	return nil
}

// liveVideo holds an optional (id, title) pair for a live/past-live video.
type liveVideo struct {
	id    string
	title string
	ok    bool
}

func saveChannel(name, chURL string, isLive bool, lv liveVideo) error {
	checkedAt := time.Now().UTC().Format(time.RFC3339Nano)
	var videoID, videoTitle interface{}
	if lv.ok {
		videoID, videoTitle = lv.id, lv.title
	}
	_, err := db.Exec(`
		INSERT INTO channels (name, url, is_live, checked_at, live_video_id, live_video_title)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(url) DO UPDATE SET
			name = excluded.name,
			is_live = excluded.is_live,
			checked_at = excluded.checked_at,
			live_video_id = excluded.live_video_id,
			live_video_title = excluded.live_video_title
	`, name, chURL, boolToInt(isLive), checkedAt, videoID, videoTitle)
	return err
}

func deleteChannel(chURL string) (bool, error) {
	res, err := db.Exec("DELETE FROM channels WHERE url = ?", chURL)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

type staleChannel struct {
	name string
	url  string
}

func getStaleChannels(maxAge time.Duration) ([]staleChannel, error) {
	cutoff := time.Now().UTC().Add(-maxAge).Format(time.RFC3339Nano)
	rows, err := db.Query("SELECT name, url FROM channels WHERE checked_at < ?", cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []staleChannel
	for rows.Next() {
		var sc staleChannel
		if err := rows.Scan(&sc.name, &sc.url); err != nil {
			return nil, err
		}
		result = append(result, sc)
	}
	return result, nil
}

var (
	videoIDRe        = regexp.MustCompile(`^/(shorts|embed|live)/([^/?#]+)`)
	channelIDRe      = regexp.MustCompile(`"channelId":"(UC[\w-]+)"`)
	handleRe         = regexp.MustCompile(`"canonicalBaseUrl":"(/@[^"]+)"`)
	authorRe         = regexp.MustCompile(`"author":"([^"]+)"`)
	canonicalLinkRe  = regexp.MustCompile(`<link rel="canonical" href="([^"]+)"`)
	isLiveNowRe      = regexp.MustCompile(`"isLiveNow":true`)
	titleRe          = regexp.MustCompile(`"title":"([^"]+)"`)
	streamsVideoRe   = regexp.MustCompile(`"videoId":"([\w-]{11})"[^}]*?"title":\{"runs":\[\{"text":"([^"]*)"`)
)

// extractVideoID pulls the video id out of common YouTube URL formats.
func extractVideoID(rawURL string) (string, bool) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", false
	}
	host := strings.ToLower(u.Host)

	if strings.Contains(host, "youtu.be") {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) > 0 && parts[0] != "" {
			return parts[0], true
		}
		return "", false
	}

	if strings.Contains(host, "youtube.com") {
		if u.Path == "/watch" {
			if v := u.Query().Get("v"); v != "" {
				return v, true
			}
		}
		if m := videoIDRe.FindStringSubmatch(u.Path); m != nil {
			return m[2], true
		}
	}

	return "", false
}

// decodeJSONString decodes a JSON-escaped string fragment (e.g. from a
// regex capture group) back into its literal form.
func decodeJSONString(s string) string {
	var out string
	if err := json.Unmarshal([]byte(`"`+s+`"`), &out); err != nil {
		return s
	}
	return out
}

func httpGet(rawURL string) (string, string, error) {
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", userAgent)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return "", "", err
	}
	finalURL := resp.Request.URL.String()
	return finalURL, string(bodyBytes), nil
}

func videoTitle(page string) string {
	if m := titleRe.FindStringSubmatch(page); m != nil {
		return decodeJSONString(m[1])
	}
	return "Untitled"
}

// fetchChannelLink resolves a video URL to its channel (name, url).
func fetchChannelLink(videoURL string) (string, string, error) {
	videoID, ok := extractVideoID(videoURL)
	if !ok {
		return "", "", fmt.Errorf("could not find a video id in that URL")
	}

	canonicalURL := "https://www.youtube.com/watch?v=" + videoID
	_, page, err := httpGet(canonicalURL)
	if err != nil {
		return "", "", fmt.Errorf("could not reach YouTube: %w", err)
	}

	channelIDMatch := channelIDRe.FindStringSubmatch(page)
	handleMatch := handleRe.FindStringSubmatch(page)
	authorMatch := authorRe.FindStringSubmatch(page)

	if channelIDMatch == nil && handleMatch == nil {
		return "", "", fmt.Errorf("could not find channel info for that video")
	}

	var channelPath string
	if handleMatch != nil {
		channelPath = handleMatch[1]
	} else {
		channelPath = "/channel/" + channelIDMatch[1]
	}

	channelURL := "https://www.youtube.com" + channelPath
	channelName := channelPath
	if authorMatch != nil {
		channelName = decodeJSONString(authorMatch[1])
	}
	return channelName, channelURL, nil
}

type liveStatus struct {
	isLive     bool
	liveVideo  liveVideo
	latestLive liveVideo
}

// fetchLiveStatus checks whether a channel is currently live, and otherwise
// finds the most recent live/premiere video from its "Live" tab.
func fetchLiveStatus(channelURL string) liveStatus {
	var status liveStatus

	// YouTube serves the live watch page's content at the /live URL without
	// an HTTP redirect; the canonical <link> tag reveals the actual video id.
	_, page, err := httpGet(channelURL + "/live")
	if err != nil {
		page = ""
	}

	var videoID string
	if m := canonicalLinkRe.FindStringSubmatch(page); m != nil {
		if u, err := url.Parse(m[1]); err == nil {
			videoID = u.Query().Get("v")
		}
	}

	isLiveNow := isLiveNowRe.MatchString(page)
	if videoID != "" && isLiveNow {
		lv := liveVideo{id: videoID, title: videoTitle(page), ok: true}
		status.isLive = true
		status.liveVideo = lv
		status.latestLive = lv
		return status
	}

	// Not currently live: look at the channel's "Live" tab for the most
	// recent past broadcast/premiere.
	_, streamsPage, err := httpGet(channelURL + "/streams")
	if err != nil {
		return status
	}

	if m := streamsVideoRe.FindStringSubmatch(streamsPage); m != nil {
		status.latestLive = liveVideo{id: m[1], title: m[2], ok: true}
	}

	return status
}

type recheckResult struct {
	name   string
	url    string
	isLive bool
}

func checkAllStaleChannels() ([]recheckResult, error) {
	stale, err := getStaleChannels(checkInterval)
	if err != nil {
		return nil, err
	}

	var results []recheckResult
	for _, sc := range stale {
		status := fetchLiveStatus(sc.url)
		if err := saveChannel(sc.name, sc.url, status.isLive, status.liveVideo); err != nil {
			return nil, err
		}
		results = append(results, recheckResult{name: sc.name, url: sc.url, isLive: status.isLive})
	}
	return results, nil
}

type channelEntry struct {
	Name           string  `json:"name"`
	URL            string  `json:"url"`
	IsLive         bool    `json:"is_live"`
	CheckedAt      string  `json:"checked_at"`
	LiveVideoURL   *string `json:"live_video_url"`
	LiveVideoTitle *string `json:"live_video_title"`
}

// getAllChannels returns all channels sorted live-first, then alphabetically
// by name.
func getAllChannels() ([]channelEntry, error) {
	rows, err := db.Query(`
		SELECT name, url, is_live, checked_at, live_video_id, live_video_title
		FROM channels
		ORDER BY is_live DESC, name COLLATE NOCASE ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var channels []channelEntry
	for rows.Next() {
		var name, chURL, checkedAt string
		var isLive int
		var liveVideoID, liveVideoTitle sql.NullString
		if err := rows.Scan(&name, &chURL, &isLive, &checkedAt, &liveVideoID, &liveVideoTitle); err != nil {
			return nil, err
		}
		entry := channelEntry{
			Name:      name,
			URL:       chURL,
			IsLive:    isLive != 0,
			CheckedAt: checkedAt,
		}
		if liveVideoTitle.Valid {
			t := liveVideoTitle.String
			entry.LiveVideoTitle = &t
		}
		if entry.IsLive && liveVideoID.Valid {
			u := "https://www.youtube.com/watch?v=" + liveVideoID.String
			entry.LiveVideoURL = &u
		}
		channels = append(channels, entry)
	}
	if channels == nil {
		channels = []channelEntry{}
	}
	return channels, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func sendHTML(w http.ResponseWriter, body string, status int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write([]byte(body))
}

func sendJSON(w http.ResponseWriter, data interface{}, status int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.Encode(data)
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	sendHTML(w, fmt.Sprintf(formPageTemplate, "", ""), http.StatusOK)
}

func handleShow(w http.ResponseWriter, r *http.Request) {
	sendHTML(w, showPage, http.StatusOK)
}

func handleShowStatus(w http.ResponseWriter, r *http.Request) {
	channels, err := getAllChannels()
	if err != nil {
		sendJSON(w, map[string]string{"error": err.Error()}, http.StatusInternalServerError)
		return
	}
	sendJSON(w, channels, http.StatusOK)
}

func handleCheckAll(w http.ResponseWriter, r *http.Request) {
	results, err := checkAllStaleChannels()
	if err != nil {
		sendHTML(w, fmt.Sprintf(formPageTemplate, "", fmt.Sprintf(
			`<div class="result error">Error: %s</div>`, html.EscapeString(err.Error()))), http.StatusInternalServerError)
		return
	}

	var body string
	if len(results) > 0 {
		var rowsHTML strings.Builder
		for _, res := range results {
			statusText := "Not live"
			if res.isLive {
				statusText = "&#128308; Live"
			}
			rowsHTML.WriteString(fmt.Sprintf(
				`<tr><td>%s</td><td><a href="%s" target="_blank" rel="noopener">%s</a></td><td>%s</td></tr>`,
				html.EscapeString(res.name), html.EscapeString(res.url), html.EscapeString(res.url), statusText,
			))
		}
		body = fmt.Sprintf(
			`<p>Re-checked %d channel(s):</p><table border="1" cellpadding="6"><tr><th>Channel</th><th>URL</th><th>Status</th></tr>%s</table>`,
			len(results), rowsHTML.String(),
		)
	} else {
		body = "<p>No channels were due for a recheck (last checked within 5 minutes).</p>"
	}

	sendHTML(w, fmt.Sprintf(formPageTemplate, "", fmt.Sprintf(`<div class="result">%s</div>`, body)), http.StatusOK)
}

func handleSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		sendHTML(w, fmt.Sprintf(formPageTemplate, "", fmt.Sprintf(
			`<div class="result error">Error: %s</div>`, html.EscapeString(err.Error()))), http.StatusOK)
		return
	}
	submittedURL := strings.TrimSpace(r.FormValue("url"))

	var resultHTML string
	if submittedURL != "" {
		channelName, channelURL, err := fetchChannelLink(submittedURL)
		if err != nil {
			resultHTML = fmt.Sprintf(`<div class="result error">Error: %s</div>`, html.EscapeString(err.Error()))
		} else {
			status := fetchLiveStatus(channelURL)
			if err := saveChannel(channelName, channelURL, status.isLive, status.liveVideo); err != nil {
				resultHTML = fmt.Sprintf(`<div class="result error">Error: %s</div>`, html.EscapeString(err.Error()))
			} else {
				var liveHTML string
				switch {
				case status.isLive:
					liveWatchURL := "https://www.youtube.com/watch?v=" + status.liveVideo.id
					liveHTML = fmt.Sprintf(
						`<p><strong>Status:</strong> &#128308; Live now &mdash; <a href="%s" target="_blank" rel="noopener">%s</a></p>`,
						html.EscapeString(liveWatchURL), html.EscapeString(status.liveVideo.title),
					)
				case status.latestLive.ok:
					watchURL := "https://www.youtube.com/watch?v=" + status.latestLive.id
					liveHTML = fmt.Sprintf(
						`<p><strong>Status:</strong> Not live. Latest live video: <a href="%s" target="_blank" rel="noopener">%s</a></p>`,
						html.EscapeString(watchURL), html.EscapeString(status.latestLive.title),
					)
				default:
					liveHTML = `<p><strong>Status:</strong> Not live, no live videos found.</p>`
				}

				resultHTML = fmt.Sprintf(
					`<div class="result"><p><strong>Channel:</strong> %s</p><p><strong>Link:</strong> <a href="%s" target="_blank" rel="noopener">%s</a></p>%s</div>`,
					html.EscapeString(channelName), html.EscapeString(channelURL), html.EscapeString(channelURL), liveHTML,
				)
			}
		}
	}

	sendHTML(w, fmt.Sprintf(formPageTemplate, html.EscapeString(submittedURL), resultHTML), http.StatusOK)
}

func handleChannelDelete(w http.ResponseWriter, r *http.Request) {
	chURL := r.URL.Query().Get("url")
	if chURL == "" {
		sendJSON(w, map[string]string{"error": "missing 'url' query parameter"}, http.StatusBadRequest)
		return
	}

	deleted, err := deleteChannel(chURL)
	if err != nil {
		sendJSON(w, map[string]string{"error": err.Error()}, http.StatusInternalServerError)
		return
	}
	if deleted {
		sendJSON(w, map[string]interface{}{"deleted": true, "url": chURL}, http.StatusOK)
	} else {
		sendJSON(w, map[string]interface{}{"deleted": false, "url": chURL}, http.StatusNotFound)
	}
}

// startBackgroundRecheck runs checkAllStaleChannels on a recurring ticker so
// channels' live status is kept fresh even if nobody has the /show page
// open to trigger a client-side recheck.
func startBackgroundRecheck() {
	go func() {
		ticker := time.NewTicker(checkInterval)
		defer ticker.Stop()
		for range ticker.C {
			if results, err := checkAllStaleChannels(); err != nil {
				log.Printf("background recheck failed: %v", err)
			} else if len(results) > 0 {
				log.Printf("background recheck updated %d channel(s)", len(results))
			}
		}
	}()
}

func main() {
	if err := initDB(); err != nil {
		log.Fatalf("failed to initialize database: %v", err)
	}
	defer db.Close()

	startBackgroundRecheck()

	mux := http.NewServeMux()
	mux.HandleFunc("/check_all", handleCheckAll)
	mux.HandleFunc("/show_status", handleShowStatus)
	mux.HandleFunc("/show", handleShow)
	mux.HandleFunc("/channel", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			handleChannelDelete(w, r)
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			handleIndex(w, r)
		case http.MethodPost:
			handleSubmit(w, r)
		default:
			http.NotFound(w, r)
		}
	})

	addr := host + ":" + port
	log.Printf("Serving on http://%s (Ctrl+C to stop)", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

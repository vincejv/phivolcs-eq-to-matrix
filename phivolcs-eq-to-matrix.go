package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

type Quake struct {
	// Date and Time when the seismic event occurred
	// Format: "02 January 2006 - 03:04 PM"
	DateTime string `json:"datetime"`
	// Approximate Latitude in decimal degrees
	Latitude string `json:"latitude"`
	// Approximate Longitude in decimal degrees
	Longitude string `json:"longitude"`
	// Depth in kilometers
	Depth string `json:"depth"`
	// Magnitude as string (e.g. "5.2")
	Magnitude string `json:"magnitude"`
	// Location description including the relative position
	Location string `json:"location"`
	// Origin location without the relative position
	Origin string `json:"origin"`
	// PHIVOLCS bulletin URL
	Bulletin string `json:"bulletin"`
}

const (
	// internal datetime format to store in cache files
	DATE_TIME_LAYOUT      = "02 January 2006 - 03:04:05 PM"
	DEFAULT_REF_POINT_LAT = 10.32
	DEFAULT_REF_POINT_LON = 123.90
	DEFAULT_REF_RADIUS_KM = 110.0
	DEFAULT_MAX_ROWS      = 500
	// file to store last fetched quakes to check if a quake needs to be updated
	CACHE_FILE = "last_quakes.json"
	// file to keep track of already posted quakes
	POST_QUAKE_FILE = "posted_quakes.json" // files to store posted matrix quakes
	// PHIVOLCS URL and defaults
	PHIVOLCS_BASE_URL = "https://earthquake.phivolcs.dost.gov.ph"
	// minimum magnitude to consider for posting even outside the refRadiusKm of refPoint
	// e.g. a strong quake far away should still be reported
	// while a weaker quake nearby should also be reported
	GLOBAL_MAG_THRESH = 4.5
	// minimum magnitude to consider when within refRadiusKm of refPoint (otherwise use globalMagThresh)
	LOCAL_MAG_THRESH = 4.0
	// Google maps URL format
	MAPS_BASE_URL = "https://www.google.com/maps?q="
	// percentage threshold for address similarity
	SIMILAR_Q_ORIGIN_THRESH = 60
	// minutes delta for similarly timed quakes
	SIMILAR_Q_MIN_DELTA_THRESH = 3
)

// ---- Configuration (from environment variables) ----
var (
	// matrix configuration from environment variables
	matrixBaseURL = os.Getenv("MATRIX_BASE_URL")     // e.g. https://matrix.example.org
	matrixRoomID  = os.Getenv("MATRIX_ROOM_ID")      // e.g. !roomid:example.org
	accessToken   = os.Getenv("MATRIX_ACCESS_TOKEN") // e.g. syt_abcdefgh123456789
	// maximum number of quake entries to parse
	maxQuakeEntries = getEnvInt("PARSE_LIMIT", DEFAULT_MAX_ROWS)
	// latitude, longitude and radius for filtering quakes when a bit below threshold
	refPointLat = getEnvFloat("REF_POINT_LAT", DEFAULT_REF_POINT_LAT)
	refPointLon = getEnvFloat("REF_POINT_LON", DEFAULT_REF_POINT_LON)
	refRadiusKm = getEnvFloat("REF_RADIUS_KM", DEFAULT_REF_RADIUS_KM)
)

// ---- Main loop ----
func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("🌋 PHIVOLCS-to-Matrix earthquake monitor started successfully ✅")
	log.Printf("Parsing up to %d quake entries from PHIVOLCS", maxQuakeEntries)

	for {
		doc, err := fetchDocument(PHIVOLCS_BASE_URL)
		if err != nil {
			log.Printf("Fetch error: %v", err)
			time.Sleep(30 * time.Second)
			continue
		}

		latestQuakes, err := parseFirstN(doc, maxQuakeEntries)
		if err != nil {
			log.Printf("Parse error: %v", err)
			time.Sleep(30 * time.Second)
			continue
		}

		// this is used to determine if a quake is new or updated
		lastFetchQuakes := readAllQuakesFromFile(CACHE_FILE, quakeOriginKey)

		// this is used to determine if a quake has already been posted to matrix
		postedQuakes := readAllQuakesFromFile(POST_QUAKE_FILE, quakeLocationKey)

		var changed []Quake
		var postedQuakesToSave []Quake
		var updated []struct {
			New Quake
			Old Quake
		}

		// parse each quake from latest fetch
		for _, currentQuake := range latestQuakes {
			// check if quake exists in last fetch (by origin and datetime)
			updatedQuakeKey := quakeOriginKey(currentQuake)
			previousQuake, updateExists := lastFetchQuakes[updatedQuakeKey]

			if !updateExists {
				if bulletinNo, _ := getBulletinNumber(currentQuake.Bulletin); bulletinNo != 1 {
					previousQuake, updateExists = determinePastQuakeThroughHeuristics(lastFetchQuakes, currentQuake)
				}
			}

			if !updateExists {
				// new quake detected
				postedQuakeKey := quakeLocationKey(currentQuake)
				_, postedExists := postedQuakes[postedQuakeKey]
				if !postedExists {
					magVal, err := strconv.ParseFloat(currentQuake.Magnitude, 64)
					threshold := magnitudeThresholdFor(currentQuake.Latitude, currentQuake.Longitude)

					if err == nil && magVal >= threshold {
						changed = append(changed, currentQuake)
						postedQuakesToSave = append(postedQuakesToSave, currentQuake)
					}
				}
			} else if quakeChanged(previousQuake, currentQuake) &&
				!updatedQuakeHasBeenPosted(postedQuakes, currentQuake) &&
				isCurrentAndPastQSignificant(currentQuake, previousQuake) {
				// updated quake detected
				updated = append(updated, struct {
					New Quake
					Old Quake
				}{currentQuake, previousQuake})
				postedQuakesToSave = append(postedQuakesToSave, currentQuake)
			}
		}

		if len(changed) == 0 && len(updated) == 0 {
			log.Println("No new or updated earthquakes detected.")
		} else {
			// Append to existing slice
			postedQuakesToSave = append(postedQuakesToSave, mapEqToSlice(postedQuakes)...)

			// Send new quakes
			for i := len(changed) - 1; i >= 0; i-- {
				q := changed[i]
				log.Printf("🆕 New quake detected: %s | M%s | %s", q.DateTime, q.Magnitude, q.Location)
				if err := postToMatrix(q, false, q); err != nil { // optional: pass q as oldQuake to avoid zero-value
					log.Printf("Matrix post failed: %v", err)
				}
			}

			// Send updated quakes
			for i := len(updated) - 1; i >= 0; i-- {
				u := updated[i]
				log.Printf("🔁 Earthquake bulletin update: %s | %s → %s | %s", u.New.DateTime, u.Old, u.New.Magnitude, u.New.Location)
				if err := postToMatrix(u.New, true, u.Old); err != nil {
					log.Printf("Matrix post failed: %v", err)
				}
			}

			// only save if there are new posts
			saveAllQuakesToFile(postedQuakesToSave, POST_QUAKE_FILE)
		}

		saveAllQuakesToFile(latestQuakes, CACHE_FILE)

		log.Println("Sleeping for 150 seconds before next poll...")
		time.Sleep(150 * time.Second)
	}
}

// --- helpers ---
// getEnvInt reads an integer environment variable and falls back to a default if not set or invalid.
func getEnvInt(envVar string, defaultVal int) int {
	val := os.Getenv(envVar)
	if val == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(val)
	if err != nil || n <= 0 {
		log.Printf("⚠️ Invalid %s value (%s), using default %d", envVar, val, defaultVal)
		return defaultVal
	}
	return n
}

// getEnvFloat reads a float environment variable and falls back to a default if not set or invalid.
func getEnvFloat(envVar string, defaultVal float64) float64 {
	val := os.Getenv(envVar)
	if val == "" {
		return defaultVal
	}
	f, err := strconv.ParseFloat(val, 64)
	if err != nil || f <= 0 {
		log.Printf("⚠️ Invalid %s value (%s), using default %.2f", envVar, val, defaultVal)
		return defaultVal
	}
	return f
}

// Fetch and parse HTML
func fetchDocument(url string) (*goquery.Document, error) {
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	client := &http.Client{Transport: tr}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("http get error: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status not OK: %s", resp.Status)
	}
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("goquery parse error: %w", err)
	}
	return doc, nil
}

// Extract datetime (in UTC) from bulletin URL if possible
func extractDateTimeFromURL(url string) (string, error) {
	// Example: https://earthquake.phivolcs.dost.gov.ph/2025_Earthquake_Information/September/2025_0930_164854_B1.html
	re := regexp.MustCompile(`(\d{4})_(\d{2})(\d{2})_(\d{6})`)
	match := re.FindStringSubmatch(url)
	if len(match) != 5 {
		return "", fmt.Errorf("no datetime in URL")
	}

	// Parse values
	year, month, day := match[1], match[2], match[3]
	hh := match[4][0:2]
	mm := match[4][2:4]
	ss := match[4][4:6]

	// Interim internal format: "2006-01-02 15:04:05" in UTC (time in URL is in UTC)
	// Note: time.Parse uses reference time "Mon Jan 2 15:04:05 MST 2006"
	// to determine the format, so we use that exact date/time in the layout.
	// We then convert to local time (Philippine time, UTC+8)
	// when formatting the final output and storing interally.
	// This is important for correct sorting and comparison of quake times.
	// PHIVOLCS Bulletin URL reports times in UTC, but we want to store in local time.
	// We assume the time in the URL is always in UTC.
	t, err := time.Parse("2006-01-02 15:04:05", fmt.Sprintf("%s-%s-%s %s:%s:%s", year, month, day, hh, mm, ss))
	if err != nil {
		return "", err
	}

	// Convert from UTC to Philippine time (+8)
	t = t.Add(8 * time.Hour)

	// Format in the desired local format
	return t.Format(DATE_TIME_LAYOUT), nil
}

// Haversine formula to calculate distance between two lat/lon points in kilometers
func distanceKm(lat1, lon1, lat2, lon2 float64) float64 {
	const earthRadiusKm = 6371.0
	dLat := (lat2 - lat1) * math.Pi / 180.0
	dLon := (lon2 - lon1) * math.Pi / 180.0
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*math.Pi/180.0)*math.Cos(lat2*math.Pi/180.0)*
			math.Sin(dLon/2)*math.Sin(dLon/2)
	return earthRadiusKm * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

// Determine magnitude threshold based on distance from reference point
func magnitudeThresholdFor(latStr, lonStr string) float64 {
	lat, err1 := strconv.ParseFloat(latStr, 64)
	lon, err2 := strconv.ParseFloat(lonStr, 64)
	if err1 != nil || err2 != nil {
		return GLOBAL_MAG_THRESH // fallback if coordinates invalid
	}

	if distanceKm(lat, lon, refPointLat, refPointLon) <= refRadiusKm {
		return LOCAL_MAG_THRESH // local threshold
	}
	return GLOBAL_MAG_THRESH // outside area
}

// Normalize date time string from PHIVOLCS raw table to ensure consistent format
func normalizeDateTime(date string) string {
	date = strings.TrimSpace(date)
	if matched, _ := regexp.MatchString(` - \d{1,2}:\d{2} [AP]M$`, date); matched {
		date = strings.Replace(date, " AM", ":00 AM", 1)
		date = strings.Replace(date, " PM", ":00 PM", 1)
	}
	return date
}

// Parse quake table
func parseFirstN(doc *goquery.Document, n int) ([]Quake, error) {
	var results []Quake
	selector := "body > div > table:nth-child(4) > tbody > tr"
	rows := doc.Find(selector)

	rows.EachWithBreak(func(i int, tr *goquery.Selection) bool {
		if i >= n {
			return false
		}
		tds := tr.Find("td")
		if tds.Length() < 6 {
			return true
		}

		link, _ := tds.Eq(0).Find("a").Attr("href")
		date := normalizeDateTime(strings.TrimSpace(tds.Eq(0).Text()))
		lat := strings.TrimSpace(tds.Eq(1).Text())
		lon := strings.TrimSpace(tds.Eq(2).Text())
		depth := strings.TrimSpace(tds.Eq(3).Text())
		mag := strings.TrimSpace(tds.Eq(4).Text())
		loc := strings.TrimSpace(strings.Join(strings.Fields(tds.Eq(5).Text()), " "))
		origin := extractOrigin(loc)

		bulletinURL := ""
		if link != "" {
			bulletinURL = fmt.Sprintf("%s/%s", PHIVOLCS_BASE_URL, strings.ReplaceAll(link, "\\", "/"))
		}

		// Attempt to parse time from bulletin URL as it is more precise
		// than the one in the table (which only has minute precision)
		// If parsing fails, fallback to the date from the table and assume ":00" seconds
		// which is still better than nothing.
		// This is important for distinguishing multiple quakes
		// that occur within the same minute.
		dateTime := date
		if bulletinURL != "" {
			if parsed, err := extractDateTimeFromURL(bulletinURL); err == nil {
				dateTime = parsed
			}
		}

		results = append(results, Quake{
			DateTime:  dateTime,
			Latitude:  lat,
			Longitude: lon,
			Depth:     depth,
			Magnitude: mag,
			Location:  loc,
			Origin:    origin,
			Bulletin:  bulletinURL,
		})
		return true
	})

	return results, nil
}

// ---- Cache handling ----
func saveAllQuakesToFile(quakes []Quake, fileName string) {
	data, _ := json.MarshalIndent(quakes, "", "  ")
	err := os.WriteFile(fileName, data, 0644)
	if err != nil {
		log.Printf("❌ Failed to write to file (%s): %v", fileName, err)
	}
}
func readAllQuakesFromFile(fileName string, keyFunc func(Quake) string) map[string]Quake {
	data, err := os.ReadFile(fileName)
	if err != nil {
		log.Printf("⚠️ File not found, starting fresh: %s", fileName)
		return map[string]Quake{}
	}

	var quakes []Quake
	if err := json.Unmarshal(data, &quakes); err != nil {
		log.Printf("⚠️ Failed to parse cache file (%s), resetting: %v", fileName, err)
		return map[string]Quake{}
	}

	m := make(map[string]Quake)
	for _, q := range quakes {
		key := keyFunc(q)
		m[key] = q
	}
	return m
}

// Determine if two quakes have the same date and time up to minute precision
// (ignoring seconds) as PHIVOLCS sometimes rounds seconds inconsistently.
func sameDateAndTimeHM(t1, t2 string) bool {
	return sameDateAndTimeHMWithDelta(t1, t2, 0)
}

// sameDateAndTimeHM returns true if two datetimes are equal up to minute precision,
// allowing a ±delta minute tolerance. Example: delta = 1 → within one minute difference.
func sameDateAndTimeHMWithDelta(t1, t2 string, delta int) bool {
	layout := DATE_TIME_LAYOUT

	d1, err1 := time.Parse(layout, t1)
	d2, err2 := time.Parse(layout, t2)
	if err1 != nil || err2 != nil {
		return false
	}

	diff := d1.Sub(d2)
	if diff < 0 {
		diff = -diff
	}

	return diff <= time.Duration(delta)*time.Minute
}

// Determine if currentQuake is a revised bulletin of pastQuake
// (same date/time up to minute precision and same origin, but higher bulletin number)
func isRevisedQuake(currentQuake, pastQ Quake) bool {
	currNum, ok1 := getBulletinNumber(currentQuake.Bulletin)
	pastNum, ok2 := getBulletinNumber(pastQ.Bulletin)

	if !ok1 || !ok2 {
		return false
	}

	return sameDateAndTimeHM(currentQuake.DateTime, pastQ.DateTime) &&
		pastQ.Origin == currentQuake.Origin &&
		currNum > pastNum
}

// Create a slice of quakes filtered by date/time (up to minute precision)
func filterQuakesByDateTime(quakes []Quake, target string) []Quake {
	var result []Quake
	for _, q := range quakes {
		if sameDateAndTimeHMWithDelta(q.DateTime, target, SIMILAR_Q_MIN_DELTA_THRESH) {
			result = append(result, q)
		}
	}
	return result
}

// Determine if currentQuake bulletin has already been posted/known
// (same date/time up to minute precision and same bulletin URL)
func isKnownBulletin(currentQuake, pastQ Quake) bool {
	return sameDateAndTimeHM(currentQuake.DateTime, pastQ.DateTime) &&
		currentQuake.Bulletin == pastQ.Bulletin
}

// Build Google Maps HTML link given latitude and longitude
func buildMapsHtmlLink(lat, lon string) string {
	return fmt.Sprintf("<a href=\"%s%s,%s\">%s°N, %s°E</a>", MAPS_BASE_URL, lat, lon, lat, lon)
}

// Build plain text coordinates string
func buildCoordinates(lat, lon string) string {
	return fmt.Sprintf("%s°N, %s°E", lat, lon)
}

// ---- Matrix posting ----
func postToMatrix(updatedQuake Quake, updated bool, oldQuake Quake) error {
	if matrixBaseURL == "" || matrixRoomID == "" || accessToken == "" {
		return fmt.Errorf("missing Matrix environment variables")
	}

	txnId := fmt.Sprintf("%d", time.Now().UnixNano()/1e6) // unique transaction ID in ms

	matrixURL := fmt.Sprintf("%s/_matrix/client/v3/rooms/%s/send/m.room.message/%s",
		strings.TrimRight(matrixBaseURL, "/"),
		matrixRoomID, // escape room ID
		url.PathEscape(txnId),
	)

	msg, formatted := formatMatrixMsg(updated, oldQuake, updatedQuake)
	payload := map[string]string{
		"msgtype":        "m.text",
		"body":           msg,
		"format":         "org.matrix.custom.html",
		"formatted_body": formatted,
	}

	data, _ := json.Marshal(payload)
	req, err := http.NewRequest("PUT", matrixURL, bytes.NewBuffer(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}

	var resp *http.Response
	var body []byte

	for attempt := 1; attempt <= 5; attempt++ {
		log.Printf("Posting to Matrix URL: %s", matrixURL)
		resp, err = client.Do(req)
		if err != nil {
			log.Printf("Matrix send attempt %d failed (network error): %v", attempt, err)
		} else {
			defer resp.Body.Close()
			body, _ = io.ReadAll(resp.Body)
			if resp.StatusCode < 300 {
				return nil // success
			}
			log.Printf("Matrix send attempt %d failed (HTTP %d): %s",
				attempt, resp.StatusCode, bytes.TrimSpace(body))
		}
		time.Sleep(time.Duration(attempt*attempt) * time.Second)
	}

	if err != nil {
		return fmt.Errorf("Matrix request failed after retries: %v", err)
	}
	return fmt.Errorf("Matrix API error: %s", string(body))
}

// Format the Matrix message based on whether it's an update or a new quake
func formatMatrixMsg(updated bool, oldQuake Quake, updatedQuake Quake) (string, string) {
	var msg, formatted string
	if updated {
		locChangedPlain := fmt.Sprintf("Location: %s", oldQuake.Location)
		locChangedHTML := fmt.Sprintf("📍 Location: %s", oldQuake.Location)
		if updatedQuake.Location != oldQuake.Location {
			locChangedPlain = fmt.Sprintf("New Location: %s\nPrevious: %s", updatedQuake.Location, oldQuake.Location)
			locChangedHTML = fmt.Sprintf("<b>📍 New Location: %s</b><br>Old: %s", updatedQuake.Location, oldQuake.Location)
		}

		magChangedPlain := oldQuake.Magnitude
		magChangedHTML := oldQuake.Magnitude
		if updatedQuake.Magnitude != oldQuake.Magnitude {
			magChangedPlain = fmt.Sprintf("%.1f → %.1f", parseMag(oldQuake.Magnitude), parseMag(updatedQuake.Magnitude))
			magChangedHTML = fmt.Sprintf("%.1f → <b>%.1f</b>", parseMag(oldQuake.Magnitude), parseMag(updatedQuake.Magnitude))
		}

		depthChangedPlain := oldQuake.Depth
		depthChangedHTML := oldQuake.Depth
		if updatedQuake.Depth != oldQuake.Depth {
			depthChangedPlain = fmt.Sprintf("%s → %s", oldQuake.Depth, updatedQuake.Depth)
			depthChangedHTML = fmt.Sprintf("%s → <b>%s</b>", oldQuake.Depth, updatedQuake.Depth)
		}

		coordChangedPlain := buildCoordinates(oldQuake.Latitude, oldQuake.Longitude)
		coordChangedHTML := buildMapsHtmlLink(oldQuake.Latitude, oldQuake.Longitude)
		if updatedQuake.Latitude != oldQuake.Latitude || updatedQuake.Longitude != oldQuake.Longitude {
			coordChangedPlain = fmt.Sprintf("%s → %s",
				buildCoordinates(oldQuake.Latitude, oldQuake.Longitude),
				buildCoordinates(updatedQuake.Latitude, updatedQuake.Longitude))
			coordChangedHTML = fmt.Sprintf("%s → <b>%s</b>",
				buildMapsHtmlLink(oldQuake.Latitude, oldQuake.Longitude),
				buildMapsHtmlLink(updatedQuake.Latitude, updatedQuake.Longitude))
		}

		msg = fmt.Sprintf(
			"💡 Earthquake Bulletin Update!\nDate & Time: %s\n%s\nMagnitude: %s\nDepth: %skm\nCoordinates: %s\nBulletin: %s\nRevised by PHIVOLCS 🔄",
			updatedQuake.DateTime, locChangedPlain, magChangedPlain, depthChangedPlain, coordChangedPlain, updatedQuake.Bulletin,
		)
		formatted = fmt.Sprintf(
			"💡 <b>Earthquake Bulletin Update!</b><br><br>📅 <b>Date & Time:</b> %s<br>%s<br>📈 <b>Magnitude:</b> %s<br>📊 <b>Depth:</b> %skm<br>🧭 <b>Coordinates:</b> %s<br>📄 <b>Bulletin:</b> <a href=\"%s\">View PHIVOLCS report</a><br><br>Revised by PHIVOLCS 🔄",
			updatedQuake.DateTime, locChangedHTML, magChangedHTML, depthChangedHTML, coordChangedHTML, updatedQuake.Bulletin,
		)
	} else {
		msg = fmt.Sprintf(
			"🚨 New Earthquake Alert!\nDate & Time: %s\nLocation: %s\nMagnitude: %.1f\nDepth: %skm\nCoordinates: %s\nBulletin: %s\nStay safe! ⚠️",
			updatedQuake.DateTime, updatedQuake.Location, parseMag(updatedQuake.Magnitude),
			updatedQuake.Depth, buildCoordinates(updatedQuake.Latitude, updatedQuake.Longitude), updatedQuake.Bulletin,
		)
		formatted = fmt.Sprintf(
			"🚨 <b>New Earthquake Alert!</b><br><br>📅 <b>Date & Time:</b> %s<br>📍 <b>Location:</b> %s<br>📈 <b>Magnitude:</b> %.1f<br>📊 <b>Depth:</b> %skm<br>🧭 <b>Coordinates:</b> %s<br>📄 <b>Bulletin:</b> <a href=\"%s\">View PHIVOLCS report</a><br><br>Stay safe! ⚠️",
			updatedQuake.DateTime, updatedQuake.Location, parseMag(updatedQuake.Magnitude),
			updatedQuake.Depth, buildMapsHtmlLink(updatedQuake.Latitude, updatedQuake.Longitude), updatedQuake.Bulletin,
		)
	}
	return msg, formatted
}

func parseMag(m string) float64 {
	v, _ := strconv.ParseFloat(m, 64)
	return v
}

func extractOrigin(fullLoc string) string {
	start := strings.Index(fullLoc, "of ")
	if start != -1 {
		// remove the "of " prefix
		mainPart := strings.TrimSpace(fullLoc[start+3:])
		return mainPart
	}
	return fullLoc
}

func quakeChanged(a, b Quake) bool {
	return a.Magnitude != b.Magnitude ||
		a.Depth != b.Depth ||
		a.Location != b.Location ||
		a.Latitude != b.Latitude ||
		a.Longitude != b.Longitude ||
		a.Bulletin != b.Bulletin
}

func quakeLocationKey(q Quake) string {
	return q.DateTime + "|" + q.Location
}

func quakeOriginKey(q Quake) string {
	return q.DateTime + "|" + q.Origin
}

func getBulletinNumber(url string) (int, bool) {
	// Regex to capture the digit after B (ignore optional F)
	re := regexp.MustCompile(`_B(\d)F?\.html$`)
	match := re.FindStringSubmatch(url)
	if len(match) > 1 {
		num, err := strconv.Atoi(match[1])
		if err == nil {
			return num, true
		}
	}
	return 0, false
}

// Remove entries older than 2 months and convert map to slice
func mapEqToSlice(m map[string]Quake) []Quake {
	var s []Quake
	now := time.Now()

	for k, v := range m {
		t, err := time.Parse(DATE_TIME_LAYOUT, v.DateTime)
		if err != nil {
			log.Printf("⚠️ Failed to parse datetime %q: %v", v.DateTime, err)
			continue
		}
		// skip entries older than 2 months
		if t.Before(now.AddDate(0, -2, 0)) {
			delete(m, k)
			continue
		}
		s = append(s, v)
	}

	// Sort by datetime (newest first)
	sort.Slice(s, func(i, j int) bool {
		ti, _ := time.Parse(DATE_TIME_LAYOUT, s[i].DateTime)
		tj, _ := time.Parse(DATE_TIME_LAYOUT, s[j].DateTime)
		return ti.After(tj)
	})

	return s
}

// updatedQuakeHasBeenPosted checks if the given currentQuake has already been posted by
// comparing it against the postedQuakes map. It returns true if a known bulletin
// matching currentQuake is found in postedQuakes, indicating that the quake has
// already been posted.
func updatedQuakeHasBeenPosted(postedQuakes map[string]Quake, currentQuake Quake) bool {
	skipPostingUpdate := false
	for _, postQ := range postedQuakes {
		if isKnownBulletin(currentQuake, postQ) {
			skipPostingUpdate = true
			break
		}
	}
	return skipPostingUpdate
}

// isCurrentAndPastQSignificant determines whether either the current or previous earthquake is considered significant
// based on their respective magnitudes and location-specific thresholds. It returns true if the magnitude
// of the current earthquake meets or exceeds the threshold for its location, or if the magnitude of the
// previous earthquake meets or exceeds the threshold for its location.
func isCurrentAndPastQSignificant(currentQuake Quake, previousQuake Quake) bool {
	thresholdForUpdatedQ := magnitudeThresholdFor(currentQuake.Latitude, currentQuake.Longitude)
	thresholdForOldQ := magnitudeThresholdFor(previousQuake.Latitude, previousQuake.Longitude)

	isSignificant := parseMag(currentQuake.Magnitude) >= thresholdForUpdatedQ ||
		parseMag(previousQuake.Magnitude) >= thresholdForOldQ
	return isSignificant
}

// Heuristic to determine if currentQuake is a revised bulletin of a past quake
// by checking similarly timed quakes and address similarity
func determinePastQuakeThroughHeuristics(lastFetchQuakes map[string]Quake, currentQuake Quake) (Quake, bool) {
	updateExists := false
	var previousQuake Quake

	for _, pastQ := range lastFetchQuakes {
		if isRevisedQuake(currentQuake, pastQ) {
			previousQuake = pastQ
			updateExists = true
			break
		}
	}

	similarlyTimedQuakes := filterQuakesByDateTime(mapEqToSlice(lastFetchQuakes), currentQuake.DateTime)
	for _, pastQ := range similarlyTimedQuakes {
		if AddressSimilarity(currentQuake.Origin, pastQ.Origin) >= SIMILAR_Q_ORIGIN_THRESH {
			curQuakeBltnNo, _ := getBulletinNumber(currentQuake.Bulletin)
			pastQuakeBltnNo, _ := getBulletinNumber(pastQ.Bulletin)
			if curQuakeBltnNo > pastQuakeBltnNo {
				previousQuake = pastQ
				updateExists = true
				break
			}

		}
	}
	return previousQuake, updateExists
}

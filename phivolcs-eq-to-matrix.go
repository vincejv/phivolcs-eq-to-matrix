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
	dateTimeLayout     = "02 January 2006 - 03:04:05 PM"
	defaultRefPointLat = 10.32
	defaultRefPointLon = 123.90
	defaultRefRadiusKm = 110.0
	defaultMaxRows     = 500
	// file to store last fetched quakes to check if a quake needs to be updated
	cacheFile = "last_quakes.json"
	// file to keep track of already posted quakes
	postQuakeFile = "posted_quakes.json" // files to store posted matrix quakes
	// PHIVOLCS URL and defaults
	phivolcsURL = "https://earthquake.phivolcs.dost.gov.ph"
	// minimum magnitude to consider for posting even outside the refRadiusKm of refPoint
	// e.g. a strong quake far away should still be reported
	// while a weaker quake nearby should also be reported
	globalMagThresh = 4.5
	// minimum magnitude to consider when within refRadiusKm of refPoint (otherwise use globalMagThresh)
	localMagThresh = 4.0
)

// ---- Configuration (from environment variables) ----
var (
	// matrix configuration from environment variables
	matrixBaseURL = os.Getenv("MATRIX_BASE_URL")     // e.g. https://matrix.example.org
	matrixRoomID  = os.Getenv("MATRIX_ROOM_ID")      // e.g. !roomid:example.org
	accessToken   = os.Getenv("MATRIX_ACCESS_TOKEN") // e.g. syt_abcdefgh123456789
	// maximum number of quake entries to parse
	maxQuakeEntries = getEnvInt("PARSE_LIMIT", defaultMaxRows)
	// latitude, longitude and radius for filtering quakes when a bit below threshold
	refPointLat = getEnvFloat("REF_POINT_LAT", defaultRefPointLat)
	refPointLon = getEnvFloat("REF_POINT_LON", defaultRefPointLon)
	refRadiusKm = getEnvFloat("REF_RADIUS_KM", defaultRefRadiusKm)
)

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
	return t.Format(dateTimeLayout), nil
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
		return globalMagThresh // fallback if coordinates invalid
	}

	if distanceKm(lat, lon, refPointLat, refPointLon) <= refRadiusKm {
		return localMagThresh // local threshold
	}
	return globalMagThresh // outside area
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
			bulletinURL = fmt.Sprintf("%s/%s", phivolcsURL, strings.ReplaceAll(link, "\\", "/"))
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

// ---- Matrix posting ----
func postToMatrix(updatedQuake Quake, updated bool, oldQuake Quake) error {
	if matrixBaseURL == "" || matrixRoomID == "" || accessToken == "" {
		return fmt.Errorf("missing Matrix environment variables")
	}

	matrixURL := fmt.Sprintf("%s/_matrix/client/v3/rooms/%s/send/m.room.message",
		strings.TrimRight(matrixBaseURL, "/"),
		matrixRoomID,
	)

	var msg, formatted string

	if updated {
		locChangedPlain := oldQuake.Location
		locChangedHTML := oldQuake.Location
		if updatedQuake.Location != oldQuake.Location {
			locChangedPlain = fmt.Sprintf("%s → %s", oldQuake.Location, updatedQuake.Location)
			locChangedHTML = fmt.Sprintf("%s → <b>%s</b>", oldQuake.Location, updatedQuake.Location)
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

		mapsLink := fmt.Sprintf("https://www.google.com/maps?q=%s,%s", oldQuake.Latitude, oldQuake.Longitude)
		coordChangedPlain := fmt.Sprintf("%s°N, %s°E", oldQuake.Latitude, oldQuake.Longitude)
		coordChangedHTML := fmt.Sprintf("<a href=\"%s\">%s°N, %s°E</a>", mapsLink, oldQuake.Latitude, oldQuake.Longitude)
		if updatedQuake.Latitude != oldQuake.Latitude || updatedQuake.Longitude != oldQuake.Longitude {
			updatedMapsLink := fmt.Sprintf("https://www.google.com/maps?q=%s,%s", updatedQuake.Latitude, updatedQuake.Longitude)
			coordChangedPlain = fmt.Sprintf("%s°N, %s°E → %s°N, %s°E", oldQuake.Latitude, oldQuake.Longitude, updatedQuake.Latitude, updatedQuake.Longitude)
			coordChangedHTML = fmt.Sprintf(
				"<a href=\"%s\">%s°N, %s°E</a> → <b><a href=\"%s\">%s°N, %s°E</a></b>",
				mapsLink, oldQuake.Latitude, oldQuake.Longitude,
				updatedMapsLink, updatedQuake.Latitude, updatedQuake.Longitude)
		}

		msg = fmt.Sprintf(
			"🔁 Earthquake Update!\n\nDate & Time: %s\nLocation: %s\nMagnitude Updated: %s\nDepth: %skm\nCoordinates: %s\nBulletin: %s\n\nRevised by PHIVOLCS ⚠️",
			updatedQuake.DateTime, locChangedPlain, magChangedPlain, depthChangedPlain, coordChangedPlain, updatedQuake.Bulletin,
		)
		formatted = fmt.Sprintf(
			"🔁 <b>Earthquake Update!</b><br><br>📅 <b>Date & Time:</b> %s<br>📍 <b>Location:</b> %s<br>📈 <b>Magnitude Updated:</b> %s<br>📊 <b>Depth:</b> %skm<br>🧭 <b>Coordinates:</b> %s<br>📄 <b>Bulletin:</b> <a href=\"%s\">View PHIVOLCS report</a><br><br>Revised by PHIVOLCS ⚠️",
			updatedQuake.DateTime, locChangedHTML, magChangedHTML, depthChangedHTML, coordChangedHTML, updatedQuake.Bulletin,
		)
	} else {
		msg = fmt.Sprintf(
			"🌏 New Earthquake Alert!\n\nDate & Time: %s\nLocation: %s\nMagnitude: %.1f\nDepth: %skm\nCoordinates: %s°N, %s°E\nBulletin: %s\n\nStay safe! ⚠️",
			updatedQuake.DateTime, updatedQuake.Location, parseMag(updatedQuake.Magnitude), updatedQuake.Depth, updatedQuake.Latitude, updatedQuake.Longitude, updatedQuake.Bulletin,
		)
		formatted = fmt.Sprintf(
			"🌏 <b>New Earthquake Alert!</b><br><br>📅 <b>Date & Time:</b> %s<br>📍 <b>Location:</b> %s<br>📈 <b>Magnitude:</b> %.1f<br>📊 <b>Depth:</b> %skm<br>🧭 <b>Coordinates:</b> <a href=\"%s\">%s°N, %s°E</a><br>📄 <b>Bulletin:</b> <a href=\"%s\">View PHIVOLCS report</a><br><br>Stay safe! ⚠️",
			updatedQuake.DateTime, updatedQuake.Location, parseMag(updatedQuake.Magnitude), updatedQuake.Depth, mapsLink, updatedQuake.Latitude, updatedQuake.Longitude, updatedQuake.Bulletin,
		)
	}

	payload := map[string]string{
		"msgtype":        "m.text",
		"body":           msg,
		"format":         "org.matrix.custom.html",
		"formatted_body": formatted,
	}

	data, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", matrixURL+"?access_token="+accessToken, bytes.NewBuffer(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("Matrix API error: %s", string(body))
	}
	return nil
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
		a.Longitude != b.Longitude
}

func quakeLocationKey(q Quake) string {
	return q.DateTime + "|" + q.Location
}

func quakeOriginKey(q Quake) string {
	return q.DateTime + "|" + q.Origin
}

// Remove entries older than 2 months and convert map to slice
func mapEqToSlice(m map[string]Quake) []Quake {
	var s []Quake
	now := time.Now()

	for k, v := range m {
		t, err := time.Parse(dateTimeLayout, v.DateTime)
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
		ti, _ := time.Parse(dateTimeLayout, s[i].DateTime)
		tj, _ := time.Parse(dateTimeLayout, s[j].DateTime)
		return ti.After(tj)
	})

	return s
}

// ---- Main loop ----
func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("🌋 PHIVOLCS-to-Matrix earthquake monitor started successfully ✅")
	log.Printf("Parsing up to %d quake entries from PHIVOLCS", maxQuakeEntries)

	for {
		url := phivolcsURL
		doc, err := fetchDocument(url)
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
		lastFetchQuakes := readAllQuakesFromFile(cacheFile, quakeOriginKey)

		// this is used to determine if a quake has already been posted to matrix
		postedQuakes := readAllQuakesFromFile(postQuakeFile, quakeLocationKey)

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
				// if not a delta update, check if already posted (by location and datetime)
				postedQuakeKey := quakeLocationKey(currentQuake)
				_, postedExists := postedQuakes[postedQuakeKey]
				if !postedExists {
					magVal, err := strconv.ParseFloat(currentQuake.Magnitude, 64)

					// determine threashold based on distance from reference point
					threshold := magnitudeThresholdFor(currentQuake.Latitude, currentQuake.Longitude)

					if err == nil && magVal >= threshold {
						// only report magnitudes greater than or equal to determined threshold
						// new earthquake recorded
						changed = append(changed, currentQuake)
						postedQuakesToSave = append(postedQuakesToSave, currentQuake)
					}
				}
			} else if quakeChanged(previousQuake, currentQuake) {
				// delta for sending update
				thresholdForUpdatedQ := magnitudeThresholdFor(currentQuake.Latitude, currentQuake.Longitude)
				thresholdForOldQ := magnitudeThresholdFor(previousQuake.Latitude, previousQuake.Longitude)

				if parseMag(currentQuake.Magnitude) >= thresholdForUpdatedQ ||
					parseMag(previousQuake.Magnitude) >= thresholdForOldQ {
					// only report updates if either the old or new magnitude is above the threshold
					// earthquake updated
					updated = append(updated, struct {
						New Quake
						Old Quake
					}{currentQuake, previousQuake})
					postedQuakesToSave = append(postedQuakesToSave, currentQuake)
				}
			}
		}

		// Append to existing slice
		postedQuakesToSave = append(postedQuakesToSave, mapEqToSlice(postedQuakes)...)

		if len(changed) == 0 && len(updated) == 0 {
			log.Println("No new or updated earthquakes detected.")
		} else {
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
				log.Printf("🔁 Magnitude update: %s | %s → %s | %s", u.New.DateTime, u.Old, u.New.Magnitude, u.New.Location)
				if err := postToMatrix(u.New, true, u.Old); err != nil {
					log.Printf("Matrix post failed: %v", err)
				}
			}

			// only save if there are new posts
			saveAllQuakesToFile(postedQuakesToSave, postQuakeFile)
		}

		saveAllQuakesToFile(latestQuakes, cacheFile)

		log.Println("Sleeping for 150 seconds before next poll...")
		time.Sleep(150 * time.Second)
	}
}

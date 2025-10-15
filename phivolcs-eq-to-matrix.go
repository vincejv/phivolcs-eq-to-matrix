package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

type Quake struct {
	DateTime  string `json:"datetime"`
	Latitude  string `json:"latitude"`
	Longitude string `json:"longitude"`
	Depth     string `json:"depth"`
	Magnitude string `json:"magnitude"`
	Location  string `json:"location"`
	Origin    string `json:"origin"`
	Bulletin  string `json:"bulletin"`
}

// ---- Configuration (from environment variables) ----
var (
	matrixBaseURL  = os.Getenv("MATRIX_BASE_URL") // e.g. https://matrix.example.org
	matrixRoomID   = os.Getenv("MATRIX_ROOM_ID")  // e.g. !roomid:example.org
	accessToken    = os.Getenv("MATRIX_ACCESS_TOKEN")
	cacheFile      = "last_quakes.json"
	phivolcsURL    = "https://earthquake.phivolcs.dost.gov.ph"
	defaultMaxRows = 100
)

// getMaxRows reads an environment variable (PARSE_LIMIT)
// and falls back to a default value if not set or invalid.
func getMaxRows() int {
	val := os.Getenv("PARSE_LIMIT")
	if val == "" {
		return defaultMaxRows
	}
	n, err := strconv.Atoi(val)
	if err != nil || n <= 0 {
		log.Printf("⚠️ Invalid PARSE_LIMIT value (%s), using default %d", val, defaultMaxRows)
		return defaultMaxRows
	}
	return n
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
		date := strings.TrimSpace(tds.Eq(0).Text())
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

		results = append(results, Quake{
			DateTime:  date,
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
func saveAllQuakes(quakes []Quake) {
	data, _ := json.MarshalIndent(quakes, "", "  ")
	err := os.WriteFile(cacheFile, data, 0644)
	if err != nil {
		log.Printf("❌ Failed to write cache file: %v", err)
	}
}

func readAllQuakes() map[string]Quake {
	data, err := os.ReadFile(cacheFile)
	if err != nil {
		log.Printf("⚠️ Cache file not found, starting fresh: %s", cacheFile)
		return map[string]Quake{}
	}
	var quakes []Quake
	if err := json.Unmarshal(data, &quakes); err != nil {
		log.Printf("⚠️ Failed to parse cache file, resetting: %v", err)
		return map[string]Quake{}
	}

	m := make(map[string]Quake)
	for _, q := range quakes {
		key := q.DateTime + "|" + q.Location
		m[key] = q
	}
	return m
}

// ---- Matrix posting ----
func postToMatrix(q Quake, updated bool, oldMag string) error {
	if matrixBaseURL == "" || matrixRoomID == "" || accessToken == "" {
		return fmt.Errorf("missing Matrix environment variables")
	}

	matrixURL := fmt.Sprintf("%s/_matrix/client/v3/rooms/%s/send/m.room.message",
		strings.TrimRight(matrixBaseURL, "/"),
		matrixRoomID,
	)

	var msg, formatted string
	mapsLink := fmt.Sprintf("https://www.google.com/maps?q=%s,%s", q.Latitude, q.Longitude)

	if updated {
		msg = fmt.Sprintf(
			"🔁 Earthquake Update!\n\nDate & Time: %s\nLocation: %s\nMagnitude Updated: %.1f → %.1f\nDepth: %skm\nCoordinates: %s°N, %s°E\nBulletin: %s\n\nRevised by PHIVOLCS ⚠️",
			q.DateTime, q.Location, parseMag(oldMag), parseMag(q.Magnitude), q.Depth, q.Latitude, q.Longitude, q.Bulletin,
		)
		formatted = fmt.Sprintf(
			"🔁 <b>Earthquake Update!</b><br><br>📅 <b>Date & Time:</b> %s<br>📍 <b>Location:</b> %s<br>📈 <b>Magnitude Updated:</b> %.1f → %.1f<br>📊 <b>Depth:</b> %skm<br>🧭 <b>Coordinates:</b> <a href=\"%s\">%s°N, %s°E</a><br>📄 <b>Bulletin:</b> <a href=\"%s\">View PHIVOLCS report</a><br><br>Revised by PHIVOLCS ⚠️",
			q.DateTime, q.Location, parseMag(oldMag), parseMag(q.Magnitude), q.Depth, mapsLink, q.Latitude, q.Longitude, q.Bulletin,
		)
	} else {
		msg = fmt.Sprintf(
			"🌏 New Earthquake Alert!\n\nDate & Time: %s\nLocation: %s\nMagnitude: %.1f\nDepth: %skm\nCoordinates: %s°N, %s°E\nBulletin: %s\n\nStay safe! ⚠️",
			q.DateTime, q.Location, parseMag(q.Magnitude), q.Depth, q.Latitude, q.Longitude, q.Bulletin,
		)
		formatted = fmt.Sprintf(
			"🌏 <b>New Earthquake Alert!</b><br><br>📅 <b>Date & Time:</b> %s<br>📍 <b>Location:</b> %s<br>📈 <b>Magnitude:</b> %.1f<br>📊 <b>Depth:</b> %skm<br>🧭 <b>Coordinates:</b> <a href=\"%s\">%s°N, %s°E</a><br>📄 <b>Bulletin:</b> <a href=\"%s\">View PHIVOLCS report</a><br><br>Stay safe! ⚠️",
			q.DateTime, q.Location, parseMag(q.Magnitude), q.Depth, mapsLink, q.Latitude, q.Longitude, q.Bulletin,
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

// ---- Main loop ----
func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("🌋 PHIVOLCS-to-Matrix earthquake monitor started successfully ✅")

	maxRows := getMaxRows()
	log.Printf("Parsing up to %d quake entries from PHIVOLCS", maxRows)

	for {
		url := phivolcsURL
		doc, err := fetchDocument(url)
		if err != nil {
			log.Printf("Fetch error: %v", err)
			time.Sleep(150 * time.Second)
			continue
		}

		quakes, err := parseFirstN(doc, maxRows)
		if err != nil {
			log.Printf("Parse error: %v", err)
			time.Sleep(150 * time.Second)
			continue
		}

		oldQuakes := readAllQuakes()
		var changed []Quake
		var updated []struct {
			New Quake
			Old string
		}

		for _, q := range quakes {
			// Use DateTime + Origin as unique key
			// WIP: need to handle multiple quakes at the same minute
			// e.g. "2024-06-01 12:34" + "100km NW of CityA"
			// vs   "2024-06-01 12:34" + "150km SE of CityB"
			// This is a temporary workaround until PHIVOLCS provides unique IDs
			// for each earthquake event.
			// For now, we assume no two quakes occur at the exact same minute
			// and location description.
			// Future improvement: parse the bulletin link for a unique ID if available.
			key := q.DateTime + "|" + q.Origin
			old, exists := oldQuakes[key]
			if !exists {
				magVal, err := strconv.ParseFloat(q.Magnitude, 64)
				if err == nil && magVal >= 4.5 {
					// only report magnitudes greater than or equal to 4.5
					// new earthquake recorded
					changed = append(changed, q)
				}
			} else if quakeChanged(old, q) {
				// delta for sending update
				updated = append(updated, struct {
					New Quake
					Old string
				}{q, old.Magnitude})
			}
		}

		if len(changed) == 0 && len(updated) == 0 {
			log.Println("No new or updated earthquakes detected.")
		} else {
			// Send new quakes
			for i := len(changed) - 1; i >= 0; i-- {
				q := changed[i]
				log.Printf("🆕 New quake detected: %s | M%s | %s", q.DateTime, q.Magnitude, q.Location)
				if err := postToMatrix(q, false, ""); err != nil {
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
		}

		saveAllQuakes(quakes)
		log.Println("Sleeping for 150 seconds before next poll...")
		time.Sleep(150 * time.Second)
	}
}

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
}

// ---- Configuration (from environment variables) ----
var (
	matrixBaseURL = os.Getenv("MATRIX_BASE_URL") // e.g. https://matrix.example.org
	matrixRoomID  = os.Getenv("MATRIX_ROOM_ID")  // e.g. !roomid:example.org
	accessToken   = os.Getenv("MATRIX_ACCESS_TOKEN")
	cacheFile     = "last_quakes.json"
)

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

		date := strings.TrimSpace(tds.Eq(0).Text())
		lat := strings.TrimSpace(tds.Eq(1).Text())
		lon := strings.TrimSpace(tds.Eq(2).Text())
		depth := strings.TrimSpace(tds.Eq(3).Text())
		mag := strings.TrimSpace(tds.Eq(4).Text())
		loc := strings.TrimSpace(strings.Join(strings.Fields(tds.Eq(5).Text()), " "))

		magVal, err := strconv.ParseFloat(mag, 64)
		if err == nil && magVal >= 4.5 {
			results = append(results, Quake{
				DateTime:  date,
				Latitude:  lat,
				Longitude: lon,
				Depth:     depth,
				Magnitude: mag,
				Location:  loc,
			})
		}
		return true
	})
	return results, nil
}

// ---- Cache handling ----
func saveAllQuakes(quakes []Quake) {
	data, _ := json.MarshalIndent(quakes, "", "  ")
	_ = os.WriteFile(cacheFile, data, 0644)
}

func readAllQuakes() map[string]Quake {
	data, err := os.ReadFile(cacheFile)
	if err != nil {
		return map[string]Quake{}
	}
	var quakes []Quake
	if err := json.Unmarshal(data, &quakes); err != nil {
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

	var msg string
	if updated {
		msg = fmt.Sprintf(
			"🔁 **Earthquake Update!**\n\n📅 **Date & Time:** %s\n📍 **Location:** %s\n📈 **Magnitude Updated:** %.1f → %.1f\n📊 **Depth:** %skm\n🧭 **Coordinates:** %s°N, %s°E\n\nRevised by PHIVOLCS ⚠️",
			q.DateTime, q.Location, parseMag(oldMag), parseMag(q.Magnitude), q.Depth, q.Latitude, q.Longitude,
		)
	} else {
		msg = fmt.Sprintf(
			"🌏 **New Earthquake Alert!**\n\n📅 **Date & Time:** %s\n📍 **Location:** %s\n📈 **Magnitude:** %.1f\n📊 **Depth:** %skm\n🧭 **Coordinates:** %s°N, %s°E\n\nStay safe! ⚠️",
			q.DateTime, q.Location, parseMag(q.Magnitude), q.Depth, q.Latitude, q.Longitude,
		)
	}

	payload := map[string]string{
		"msgtype":        "m.text",
		"body":           msg,
		"format":         "org.matrix.custom.html",
		"formatted_body": strings.ReplaceAll(msg, "\n", "<br>"),
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

// ---- Main loop ----
func main() {
	for {
		url := "https://earthquake.phivolcs.dost.gov.ph/"
		doc, err := fetchDocument(url)
		if err != nil {
			log.Printf("Fetch error: %v", err)
			time.Sleep(150 * time.Second)
			continue
		}

		quakes, err := parseFirstN(doc, 100)
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
			key := q.DateTime + "|" + q.Location
			old, exists := oldQuakes[key]
			if !exists {
				changed = append(changed, q)
			} else if old.Magnitude != q.Magnitude {
				updated = append(updated, struct {
					New Quake
					Old string
				}{q, old.Magnitude})
			}
		}

		// Send new quakes
		for i := len(changed) - 1; i >= 0; i-- {
			q := changed[i]
			log.Printf("Posting NEW quake: %+v\n", q)
			if err := postToMatrix(q, false, ""); err != nil {
				log.Printf("Matrix post failed: %v", err)
			} else {
				log.Println("Posted new quake successfully ✅")
			}
		}

		// Send updated quakes
		for i := len(updated) - 1; i >= 0; i-- {
			u := updated[i]
			log.Printf("Posting UPDATED quake: %+v (old %s)\n", u.New, u.Old)
			if err := postToMatrix(u.New, true, u.Old); err != nil {
				log.Printf("Matrix post failed: %v", err)
			} else {
				log.Println("Posted updated quake successfully 🔁")
			}
		}

		saveAllQuakes(quakes)
		time.Sleep(150 * time.Second)
	}
}

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
	DateTime  string
	Latitude  string
	Longitude string
	Depth     string
	Magnitude string
	Location  string
}

// ---- Configuration (from environment variables) ----
var (
	matrixBaseURL = os.Getenv("MATRIX_BASE_URL") // e.g. https://matrix.example.org
	matrixRoomID  = os.Getenv("MATRIX_ROOM_ID")  // e.g. !roomid:example.org
	accessToken   = os.Getenv("MATRIX_ACCESS_TOKEN")
	cacheFile     = "last_quake.txt"
)

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

func readLastQuake() string {
	data, err := os.ReadFile(cacheFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func saveLastQuake(ts string) {
	_ = os.WriteFile(cacheFile, []byte(ts), 0644)
}

func postToMatrix(q Quake) error {
	if matrixBaseURL == "" || matrixRoomID == "" || accessToken == "" {
		return fmt.Errorf("missing Matrix environment variables")
	}

	matrixURL := fmt.Sprintf("%s/_matrix/client/v3/rooms/%s/send/m.room.message",
		strings.TrimRight(matrixBaseURL, "/"),
		matrixRoomID,
	)

	msg := fmt.Sprintf(
		"🌏 **Earthquake Alert!**\n\n📅 **Date & Time:** %s\n📍 **Location:** %s\n📈 **Magnitude:** %.1f\n📊 **Depth:** %skm\n🧭 **Coordinates:** %s°N, %s°E\n\nStay safe! ⚠️",
		q.DateTime, q.Location, parseMag(q.Magnitude), q.Depth, q.Latitude, q.Longitude,
	)

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

	body, err := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("Matrix API error: %s", string(body))
	}
	return nil
}

func parseMag(m string) float64 {
	v, _ := strconv.ParseFloat(m, 64)
	return v
}

func main() {
	for {
		url := "https://earthquake.phivolcs.dost.gov.ph/"
		doc, err := fetchDocument(url)
		if err != nil {
			log.Fatalf("Failed to fetch document: %v", err)
		}

		quakes, err := parseFirstN(doc, 100)
		if err != nil {
			log.Fatalf("Parse error: %v", err)
		}

		lastStored := readLastQuake()
		var newQuakes []Quake

		for _, q := range quakes {
			if q.DateTime == lastStored {
				break
			}
			newQuakes = append(newQuakes, q)
		}

		if len(newQuakes) == 0 {
			log.Println("No new earthquake detected above magnitude 4.5.")
			return
		}

		for i := len(newQuakes) - 1; i >= 0; i-- {
			q := newQuakes[i]
			fmt.Printf("Posting: %+v\n", q)
			if err := postToMatrix(q); err != nil {
				log.Printf("Matrix post failed: %v", err)
			} else {
				log.Println("Posted to Matrix successfully ✅")
			}
		}

		saveLastQuake(quakes[0].DateTime)

		time.Sleep(150 * time.Second)
	}
}

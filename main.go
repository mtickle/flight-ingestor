package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os" // Added for environment variables
	"strconv"
	"sync"
	"time"

	"github.com/joho/godotenv" // Added for .env files
)

// --- Configuration ---
const (
	// --- API Parameters for fetching ---
	apiLat      = 35.740971
	apiLng      = -78.498878
	apiRadiusNM = 50 // Your large fetch radius (e.g., 50)

	// --- Proximity Alert Zone ---
	proximityRadiusNM   = 5.0
	proximityAltitudeFT = 2000.0
	earthRadiusNM       = 3440.065

	// --- Other Consts ---
	adsbdbAPIURL      = "https://api.adsbdb.com/v0/aircraft/" // Append {HEX}
	watchlistCSVURL   = "https://raw.githubusercontent.com/sdr-enthusiasts/plane-alert-db/main/plane-alert-db-images.csv"
	pollInterval      = 60 * time.Second
	watchlistInterval = 24 * time.Hour
)

// --- Global Variables ---
var (
	adsbAPIURL = fmt.Sprintf("https://api.adsb.lol/v2/point/%.6f/%.6f/%d", apiLat, apiLng, apiRadiusNM)

	// --- Loaded from .env in main() ---
	discordWebhookURL string
)

// --- Structs for ADSB.lol API (Sightings) ---
type ADSBResponse struct {
	Aircraft []Aircraft `json:"ac"`
}
type Aircraft struct {
	Hex     string  `json:"hex"`
	Flight  string  `json:"flight"`
	NNumber string  `json:"r"`
	Squawk  string  `json:"squawk"`
	Mil     bool    `json:"mil"`
	AltBaro any     `json:"alt_baro"`
	GS      float64 `json:"gs"`
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
}

// --- Structs for ADSBdb.com (still needed for parsing) ---
type AircraftDetail struct {
	Hex          string `json:"hex"`
	Registration string `json:"registration"`
	Airline      string `json:"airline"`
	Owner        string `json:"owner"`
	AircraftType string `json:"type"`
	Note         string `json:"note"`
	ThumbnailURL string
	FullImageURL string
}

type AdsbDbApiResponse struct {
	Response struct {
		Aircraft struct {
			Type         string `json:"type"`
			Registration string `json:"registration"`
			Owner        string `json:"registered_owner"`
			AirlineFlag  string `json:"registered_owner_operator_flag_code"`
			ThumbnailURL string `json:"url_photo_thumbnail"`
			FullImageURL string `json:"url_photo"`
		} `json:"aircraft"`
	} `json:"response"`
}

// --- Structs for Watchlist (CSV) ---
type WatchlistEntry struct {
	ICAO         string
	Registration string
	Note         string
	PlaneType    string
}

// --- Structs for Discord ---
type DiscordWebhook struct {
	Embeds []Embed `json:"embeds"`
}
type Embed struct {
	Title       string  `json:"title"`
	Description string  `json:"description,omitempty"`
	Color       int     `json:"color"`
	Fields      []Field `json:"fields"`
	URL         string  `json:"url"`
	Footer      Footer  `json:"footer"`
	Image       Image   `json:"image,omitempty"` // Using large image
}
type Image struct {
	URL string `json:"url"`
}
type Field struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}
type Footer struct {
	Text string `json:"text"`
}

// --- State Management ---
type AircraftState struct {
	LastSquawk       string
	MilAlerted       bool
	WatchlistAlerted bool
	ProximityAlerted bool
	LastSeen         time.Time
}

var globalAircraftState = make(map[string]AircraftState)

// --- Watchlist Management ---
var (
	globalWatchlist = make(map[string]WatchlistEntry)
	watchlistMutex  = &sync.RWMutex{}
)

// --- Main Application ---
func main() {
	fmt.Println("Starting ADSB Alerter (No-DB Version)...")

	// --- Load .env file ---
	err := godotenv.Load()
	if err != nil {
		fmt.Println("Warning: Could not find .env file, reading from environment.")
	}

	// --- Load Discord webhook from env ---
	discordWebhookURL = os.Getenv("DISCORD_WEBHOOK_URL")
	if discordWebhookURL == "" {
		fmt.Println("FATAL: DISCORD_WEBHOOK_URL not set in environment")
		return
	}

	// --- NOTE: initDB() call is GONE ---
	fmt.Println("Database connection is disabled.")

	// --- Load watchlist in a separate goroutine ---
	go manageWatchlist()

	// Start main polling loop
	mainLoop()
}

// --- NOTE: initDB() function is GONE ---

// --- Goroutine to manage fetching the watchlist ---
func manageWatchlist() {
	ticker := time.NewTicker(watchlistInterval)
	defer ticker.Stop()

	loadWatchlistFromCSV := func() {
		fmt.Println("Refreshing aircraft watchlist from GitHub...")
		resp, err := http.Get(watchlistCSVURL)
		if err != nil {
			fmt.Printf("Error fetching watchlist CSV: %v\n", err)
			return
		}
		defer resp.Body.Close()

		reader := csv.NewReader(resp.Body)
		records, err := reader.ReadAll()
		if err != nil {
			fmt.Printf("Error parsing watchlist CSV: %v\n", err)
			return
		}

		newWatchlist := make(map[string]WatchlistEntry)
		for i, row := range records {
			if i == 0 {
				continue
			}
			if len(row) > 6 {
				entry := WatchlistEntry{
					ICAO:         row[0],
					Registration: row[1],
					PlaneType:    row[4],
					Note:         row[6],
				}
				newWatchlist[entry.ICAO] = entry
			}
		}

		watchlistMutex.Lock()
		globalWatchlist = newWatchlist
		watchlistMutex.Unlock()
		fmt.Printf("Successfully loaded %d aircraft into watchlist.\n", len(globalWatchlist))
	}

	loadWatchlistFromCSV()
	for range ticker.C {
		loadWatchlistFromCSV()
	}
}

// --- Main processing loop ---
func mainLoop() {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		fetchAndProcessAircraft()
		fmt.Printf("...Waiting for next poll in %v\n", pollInterval)
		<-ticker.C
	}
}

// --- Helper Functions ---
func formatAltitudeString(alt any) string {
	switch v := alt.(type) {
	case float64:
		return fmt.Sprintf("%.0f", v)
	case string:
		return v
	default:
		return "N/A"
	}
}

func haversine(lat1, lon1, lat2, lon2 float64) float64 {
	radLat1 := lat1 * math.Pi / 180
	radLon1 := lon1 * math.Pi / 180
	radLat2 := lat2 * math.Pi / 180
	radLon2 := lon2 * math.Pi / 180

	dLon := radLon2 - radLon1
	dLat := radLat2 - radLat1

	a := math.Pow(math.Sin(dLat/2), 2) + math.Cos(radLat1)*math.Cos(radLat2)*math.Pow(math.Sin(dLon/2), 2)
	c := 2 * math.Asin(math.Sqrt(a))

	return c * earthRadiusNM
}

// --- Core Logic ---
func fetchAndProcessAircraft() {
	fmt.Println("Fetching new aircraft data...")

	resp, err := http.Get(adsbAPIURL)
	if err != nil {
		fmt.Printf("Error fetching ADSB data: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Printf("ADSB API returned non-200 status: %s\n", resp.Status)
		return
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Printf("Error reading response body: %v\n", err)
		return
	}

	var data ADSBResponse
	if err := json.Unmarshal(bodyBytes, &data); err != nil {
		fmt.Printf("Error decoding JSON: %v\n", err)
		return
	}

	fmt.Printf("Processing %d aircraft...\n", len(data.Aircraft))

	for _, ac := range data.Aircraft {
		// --- NOTE: saveSightingToPostgres() call is GONE ---

		// Check if this aircraft triggers an alert
		processAircraftAlerts(ac)
	}

	cleanupOldState()
}

func processAircraftAlerts(ac Aircraft) {
	hex := ac.Hex
	squawk := ac.Squawk
	currentState, seen := globalAircraftState[hex]
	isEmergency := (squawk == "7700" || squawk == "7600" || squawk == "7500")

	// --- Trigger 1: Watchlist Hit (Highest Priority) ---
	watchlistMutex.RLock()
	entry, onWatchlist := globalWatchlist[hex]
	watchlistMutex.RUnlock()

	if onWatchlist {
		if !seen || !currentState.WatchlistAlerted {
			fmt.Printf("!!! WATCHLIST DETECTED: %s (Note: %s)\n", hex, entry.Note)

			details, err := getAircraftDetails(hex) // Fetch on-demand
			if err != nil {
				fmt.Printf("Error getting details for %s: %v\n", hex, err)
			}
			sendDiscordAlert(ac, details, "watchlist", &entry)

			currentState.WatchlistAlerted = true
		}
		currentState.LastSquawk = squawk
		currentState.LastSeen = time.Now()
		globalAircraftState[hex] = currentState
		return
	}

	// --- Trigger 2: Emergency Squawk ---
	if isEmergency {
		if !seen || currentState.LastSquawk != squawk {
			fmt.Printf("!!! EMERGENCY DETECTED: %s (Flight: %s) squawking %s\n", hex, ac.Flight, squawk)

			details, err := getAircraftDetails(hex) // Fetch on-demand
			if err != nil {
				fmt.Printf("Error getting details for %s: %v\n", hex, err)
			}
			sendDiscordAlert(ac, details, "emergency", nil)
		}
		currentState.LastSquawk = squawk
		currentState.LastSeen = time.Now()
		globalAircraftState[hex] = currentState
		return
	}

	// --- Trigger 3: Military Aircraft ---
	if ac.Mil {
		if !seen || !currentState.MilAlerted {
			fmt.Printf("!!! MILITARY DETECTED: %s (Flight: %s)\n", hex, ac.Flight)

			details, err := getAircraftDetails(hex) // Fetch on-demand
			if err != nil {
				fmt.Printf("Error getting details for %s: %v\n", hex, err)
			}
			sendDiscordAlert(ac, details, "military", nil)
			currentState.MilAlerted = true
		}
		currentState.LastSquawk = squawk
		currentState.LastSeen = time.Now()
		globalAircraftState[hex] = currentState
		return
	}

	// --- Trigger 4: Proximity "Overhead" Alert ---
	distanceNM := haversine(apiLat, apiLng, ac.Lat, ac.Lon)

	if distanceNM <= proximityRadiusNM {
		altStr := formatAltitudeString(ac.AltBaro)
		altitudeFT, err := strconv.ParseFloat(altStr, 64)

		if err == nil && altitudeFT > 0 && altitudeFT <= proximityAltitudeFT {
			if !seen || !currentState.ProximityAlerted {
				fmt.Printf("!!! PROXIMITY DETECTED: %s (%.1f nm, %.0f ft)\n", ac.Hex, distanceNM, altitudeFT)

				details, _ := getAircraftDetails(hex) // Fetch on-demand
				sendDiscordAlert(ac, details, "proximity", nil)

				currentState.ProximityAlerted = true
			}
		} else {
			currentState.ProximityAlerted = false
		}
	} else {
		currentState.ProximityAlerted = false
	}

	// --- No Alert: Just update state ---
	currentState.LastSquawk = squawk
	currentState.LastSeen = time.Now()
	globalAircraftState[hex] = currentState
}

func cleanupOldState() {
	cutoff := time.Now().Add(-30 * time.Minute)
	removedCount := 0

	keysToDelete := []string{}
	for hex, state := range globalAircraftState {
		if state.LastSeen.IsZero() {
			globalAircraftState[hex] = AircraftState{LastSeen: time.Now()}
		} else if state.LastSeen.Before(cutoff) {
			keysToDelete = append(keysToDelete, hex)
		}
	}

	for _, hex := range keysToDelete {
		delete(globalAircraftState, hex)
		removedCount++
	}

	if removedCount > 0 {
		fmt.Printf("State cleanup complete. Removed %d old aircraft. Tracking %d.\n", removedCount, len(globalAircraftState))
	}
}

// --- NOTE: saveSightingToPostgres() function is GONE ---

// --- UPDATED: getAircraftDetails (No Cache) ---
// This function now fetches from the API every single time.
func getAircraftDetails(hex string) (AircraftDetail, error) {
	var detail AircraftDetail

	// --- Step 1: No cache, always fetch from API ---
	fmt.Printf("API FETCH: Fetching details for %s from adsbdb.com\n", hex)
	apiURL := adsbdbAPIURL + hex
	resp, err := http.Get(apiURL)
	fmt.Println(apiURL)
	if err != nil {
		return detail, fmt.Errorf("API fetch error for %s: %v", hex, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return detail, fmt.Errorf("adsbdb API returned non-200 status: %s", resp.Status)
	}

	var apiResponse AdsbDbApiResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResponse); err != nil {
		return detail, fmt.Errorf("API JSON decode error for %s: %v", hex, err)
	}

	// --- Manually map fields ---
	detail.Hex = hex
	detail.Registration = apiResponse.Response.Aircraft.Registration
	detail.AircraftType = apiResponse.Response.Aircraft.Type
	detail.Owner = apiResponse.Response.Aircraft.Owner
	detail.ThumbnailURL = apiResponse.Response.Aircraft.ThumbnailURL
	detail.FullImageURL = apiResponse.Response.Aircraft.FullImageURL

	if apiResponse.Response.Aircraft.AirlineFlag != "" {
		detail.Airline = apiResponse.Response.Aircraft.AirlineFlag
	} else {
		detail.Airline = apiResponse.Response.Aircraft.Owner // Fallback
	}

	// --- Step 3: No cache, so no DB save ---
	return detail, nil
}

// --- Discord Alerting ---
func sendDiscordAlert(ac Aircraft, details AircraftDetail, alertType string, entry *WatchlistEntry) {
	if discordWebhookURL == "" {
		fmt.Println("Discord webhook URL not loaded. Skipping alert.")
		return
	}

	var title, description string
	var color int
	altStr := formatAltitudeString(ac.AltBaro)

	switch alertType {
	case "watchlist":
		title = "⭐️ Watchlist Alert"
		description = fmt.Sprintf("**Note:** %s", entry.Note)
		color = 16776960 // Yellow
	case "emergency":
		title = "🔴 EMERGENCY"
		description = fmt.Sprintf("Squawking: **%s**", ac.Squawk)
		color = 16711680 // Red
	case "military":
		title = "✈️ Military Aircraft Detected"
		color = 3447003 // Blue
	case "proximity":
		title = "📡 LOW-FLYING: Overhead Alert"
		description = fmt.Sprintf("**Aircraft is at %s ft within 5nm**", altStr)
		color = 16753920 // Orange
	}

	if details.FullImageURL != "" {
		description = fmt.Sprintf("[View Full Image](%s)\n%s", details.FullImageURL, description)
	}

	fields := []Field{
		{Name: "Callsign", Value: fmt.Sprintf("`%s`", ac.Flight), Inline: true},
		{Name: "ICAO Hex", Value: fmt.Sprintf("`%s`", ac.Hex), Inline: true},
		{Name: "Squawk", Value: fmt.Sprintf("`%s`", ac.Squawk), Inline: true},
		{Name: "Registration", Value: fmt.Sprintf("`%s`", details.Registration), Inline: true},
		{Name: "Aircraft Type", Value: fmt.Sprintf("`%s`", details.AircraftType), Inline: true},
		{Name: "Altitude", Value: fmt.Sprintf("%s ft", altStr), Inline: true},
		{Name: "Speed", Value: fmt.Sprintf("%.1f kts", ac.GS), Inline: true},
		{Name: "Owner", Value: details.Owner, Inline: false},
		{Name: "Airline", Value: details.Airline, Inline: false},
	}

	embed := Embed{
		Title:       title,
		Description: description,
		Color:       color,
		URL:         fmt.Sprintf("https://globe.adsb.lol/?icao=%s", ac.Hex),
		Fields:      fields,
		Footer:      Footer{Text: "ADSB.lol Alerter (No-DB)"},
	}

	// Use the large image slot
	if details.FullImageURL != "" {
		embed.Image = Image{URL: details.FullImageURL}
	}

	payload, _ := json.Marshal(DiscordWebhook{Embeds: []Embed{embed}})
	resp, err := http.Post(discordWebhookURL, "application/json", bytes.NewBuffer(payload))
	if err != nil {
		fmt.Printf("Error sending Discord alert: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		fmt.Printf("Discord API returned non-2xx status: %s\n", resp.Status)
	} else {
		fmt.Printf("Successfully sent Discord alert for %s (Type: %s)\n", ac.Hex, alertType)
	}
}

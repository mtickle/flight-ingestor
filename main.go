package main

import (
	"bytes"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os" // <-- Added for environment variables
	"strconv"
	"sync"
	"time"

	"github.com/joho/godotenv" // <-- Added for .env files
	_ "github.com/lib/pq"
)

//--- Configuration

const (
	//--- API Parameters for fetching
	apiLat      = 35.740971
	apiLng      = -78.498878
	apiRadiusNM = 50

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
	db         *sql.DB // Global database connection pool
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

// --- Structs for ADSBdb.com (Cache) ---
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

// --- NEW ---
// AdsbDbApiResponse matches the *exact* nested structure from adsbdb.com
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
	Title       string    `json:"title"`
	Description string    `json:"description,omitempty"`
	Color       int       `json:"color"`
	Fields      []Field   `json:"fields"`
	URL         string    `json:"url"`
	Footer      Footer    `json:"footer"`
	Thumbnail   Thumbnail `json:"thumbnail,omitempty"` // <-- ADD THIS
}

// --- ADD THIS NEW STRUCT ---
type Thumbnail struct {
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

// AircraftState stores what we've seen and alerted on
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
	fmt.Println("Starting ADSB Alerter...")

	// --- NEW: Load .env file ---
	err := godotenv.Load()
	if err != nil {
		fmt.Println("Warning: Could not find .env file, reading from environment.")
	}

	// --- NEW: Load Discord webhook from env ---
	discordWebhookURL = os.Getenv("DISCORD_WEBHOOK_URL")
	if discordWebhookURL == "" {
		fmt.Println("FATAL: DISCORD_WEBHOOK_URL not set in environment")
		return
	}

	// --- Connect to Postgres ---
	if err := initDB(); err != nil {
		fmt.Printf("FATAL: Failed to connect to database: %v\n", err)
		return // Exit if DB connection fails
	}
	fmt.Println("Successfully connected to PostgreSQL.")

	// --- Load watchlist in a separate goroutine ---
	go manageWatchlist()

	// Start main polling loop
	mainLoop()
}

// --- NEW: Database Initialization ---
// --- UPDATED: Database Initialization ---
func initDB() error {
	// Read the individual components from the .env file
	user := os.Getenv("DATABASE_USERNAME")
	host := os.Getenv("DATABASE_HOST")
	dbname := os.Getenv("DATABASE_NAME")
	password := os.Getenv("DATABASE_PASSWORD")
	port := os.Getenv("DATABASE_PORT")

	// Check that all required variables are loaded
	if user == "" || host == "" || dbname == "" || password == "" || port == "" {
		return fmt.Errorf("missing one or more required database environment variables")
	}

	// Construct the connection string
	// Using key=value format is safer for passwords with special characters.
	// We use "sslmode=require" because aivencloud.com is a remote, secure host.
	connStr := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=require",
		host,
		port,
		user,
		password,
		dbname,
	)

	var err error
	db, err = sql.Open("postgres", connStr)
	if err != nil {
		return err
	}

	// Ping the database to ensure the connection is live
	if err = db.Ping(); err != nil {
		return err
	}
	return nil
}

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
		// For this CSV, icao=0, registration=1, plane_type=4, note=6
		for i, row := range records {
			if i == 0 { // Skip header row
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

	// Run immediately on start, then wait for ticker
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

// formatAltitudeString safely handles the "any" type from ac.AltBaro
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

// haversine calculates the distance between two lat/lon points in nautical miles
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

	// 1. Fetch Data from ADSB.lol
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

	// 2. Process Each Aircraft
	for _, ac := range data.Aircraft {
		// Save every sighting to the log
		saveSightingToPostgres(ac)

		// Check if this aircraft triggers an alert
		processAircraftAlerts(ac)
	}

	// 3. Clean up old state
	cleanupOldState()
}

// processAircraftAlerts checks a single aircraft against our rules
func processAircraftAlerts(ac Aircraft) {
	hex := ac.Hex
	squawk := ac.Squawk
	currentState, seen := globalAircraftState[hex]
	isEmergency := (squawk == "7700" || squawk == "7600" || squawk == "7500")

	// --- Trigger 1: Watchlist Hit (Highest Priority) ---
	watchlistMutex.RLock() // Lock for reading
	entry, onWatchlist := globalWatchlist[hex]
	watchlistMutex.RUnlock() // Unlock

	if onWatchlist {
		if !seen || !currentState.WatchlistAlerted {
			fmt.Printf("!!! WATCHLIST DETECTED: %s (Note: %s)\n", hex, entry.Note)

			details, err := getAircraftDetails(hex)
			if err != nil {
				fmt.Printf("Error getting details for %s: %v\n", hex, err)
			}
			sendDiscordAlert(ac, details, "watchlist", &entry)

			currentState.WatchlistAlerted = true
		}
		// Update state and return regardless
		currentState.LastSquawk = squawk
		currentState.LastSeen = time.Now()
		globalAircraftState[hex] = currentState
		return
	}

	// --- Trigger 2: Emergency Squawk ---
	if isEmergency {
		if !seen || currentState.LastSquawk != squawk {
			fmt.Printf("!!! EMERGENCY DETECTED: %s (Flight: %s) squawking %s\n", hex, ac.Flight, squawk)

			details, err := getAircraftDetails(hex)
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

			details, err := getAircraftDetails(hex)
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
		// Inside the 5nm zone. Check altitude.
		altStr := formatAltitudeString(ac.AltBaro)
		altitudeFT, err := strconv.ParseFloat(altStr, 64)

		if err == nil && altitudeFT > 0 && altitudeFT <= proximityAltitudeFT {
			// This is a valid "overhead" plane.
			if !seen || !currentState.ProximityAlerted {
				fmt.Printf("!!! PROXIMITY DETECTED: %s (%.1f nm, %.0f ft)\n", ac.Hex, distanceNM, altitudeFT)

				details, _ := getAircraftDetails(hex)
				sendDiscordAlert(ac, details, "proximity", nil)

				currentState.ProximityAlerted = true
			}
		} else {
			// Inside 5nm zone, but too high (or on ground). Reset flag.
			currentState.ProximityAlerted = false
		}
	} else {
		// --- Plane is OUTSIDE the 5nm zone ---
		// Reset the proximity flag so it can re-trigger if it enters.
		currentState.ProximityAlerted = false
	}

	// --- No Alert: Just update state ---
	currentState.LastSquawk = squawk
	currentState.LastSeen = time.Now()
	globalAircraftState[hex] = currentState
}

// cleanupOldState removes aircraft from memory if not seen for a while
func cleanupOldState() {
	cutoff := time.Now().Add(-30 * time.Minute) // 30-minute cutoff
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

// --- Database Functions ---

// saveSightingToPostgres logs every aircraft sighting
func saveSightingToPostgres(ac Aircraft) {
	if db == nil {
		fmt.Println("DB is nil, skipping sighting save.")
		return
	}

	altStr := formatAltitudeString(ac.AltBaro)

	sql := `INSERT INTO aircraft_sightings (hex, flight, n_number, squawk, mil, alt_baro, gs, lat, lon)
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`

	_, err := db.Exec(sql, ac.Hex, ac.Flight, ac.NNumber, ac.Squawk, ac.Mil, altStr, ac.GS, ac.Lat, ac.Lon)
	if err != nil {
		fmt.Printf("DB_SIGHTING_SAVE ERROR for %s: %v\n", ac.Hex, err)
	}
}

// getAircraftDetails fetches rich data, using the DB as a cache
func getAircraftDetails(hex string) (AircraftDetail, error) {
	var detail AircraftDetail
	if db == nil {
		fmt.Println("DB is nil, cannot get aircraft details.")
		return detail, fmt.Errorf("database not connected")
	}

	// --- Step 1: Query your Postgres Cache ---
	// UPDATED: Added thumbnail_url to the SELECT
	row := db.QueryRow("SELECT hex, registration, airline, owner, aircraft_type, note, thumbnail_url FROM aircraft_details WHERE hex = $1", hex)
	// UPDATED: Added &detail.ThumbnailURL to the Scan
	err := row.Scan(&detail.Hex, &detail.Registration, &detail.Airline, &detail.Owner, &detail.AircraftType, &detail.Note, &detail.ThumbnailURL)

	if err == nil {
		// Found in cache!
		fmt.Printf("CACHE HIT: Found details for %s in DB\n", hex)
		return detail, nil
	}

	if err != sql.ErrNoRows {
		// A real database error occurred
		return detail, fmt.Errorf("DB query error for %s: %v", hex, err)
	}

	// --- Step 2: Not in cache (sql.ErrNoRows), so fetch from API ---
	fmt.Printf("CACHE MISS: Fetching details for %s from adsbdb.com\n", hex)
	apiURL := adsbdbAPIURL + hex
	resp, err := http.Get(apiURL)
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
	detail.ThumbnailURL = apiResponse.Response.Aircraft.ThumbnailURL // <-- ADD THIS

	if apiResponse.Response.Aircraft.AirlineFlag != "" {
		detail.Airline = apiResponse.Response.Aircraft.AirlineFlag
	} else {
		detail.Airline = apiResponse.Response.Aircraft.Owner // Fallback
	}

	// --- Step 3: Save new data to cache ---
	// UPDATED: Added thumbnail_url to the INSERT
	_, err = db.Exec(`INSERT INTO aircraft_details (hex, registration, airline, owner, aircraft_type, note, thumbnail_url, last_fetched_at)
	                  VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
	                  ON CONFLICT (hex) DO UPDATE SET 
	                  registration = $2, airline = $3, owner = $4, aircraft_type = $5, note = $6, thumbnail_url = $7, last_fetched_at = NOW()`,
		detail.Hex, detail.Registration, detail.Airline, detail.Owner, detail.AircraftType, detail.Note, detail.ThumbnailURL)

	if err != nil {
		fmt.Printf("DB CACHE_SAVE ERROR for %s: %v\n", hex, err)
	}

	return detail, nil
}

// --- Discord Alerting ---

// sendDiscordAlert formats and sends the webhook
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
		title = fmt.Sprintf("🔴 EMERGENCY: SQUAWK %s", ac.Squawk)
		color = 16711680 // Red
	case "military":
		title = "✈️ Military Aircraft Detected"
		color = 3447003 // Blue
	case "proximity":
		title = "📡 LOW-FLYING: Overhead Alert"
		description = fmt.Sprintf("**Aircraft is at %s ft within 5nm**", altStr)
		color = 16753920 // Orange
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

	// Create the base embed
	embed := Embed{
		Title:       title,
		Description: description,
		Color:       color,
		URL:         fmt.Sprintf("https://globe.adsb.lol/?icao=%s", ac.Hex),
		Fields:      fields,
		Footer:      Footer{Text: "ADSB.lol Alerter (Refined)"},
	}

	// --- ADD THIS BLOCK ---
	// If a thumbnail URL exists in our cache, add it to the embed
	if details.ThumbnailURL != "" {
		embed.Thumbnail = Thumbnail{URL: details.ThumbnailURL}
	}
	// ---------------------

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

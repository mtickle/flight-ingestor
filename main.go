package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// --- Configuration
const (
	//--- Discord Webhooks
	discordHookWatchlist  = "https://discord.com/api/webhooks/1436316981333725205/PvSP1_13ynSKkyD1r8dCp4q5tbHk4l1d9d_4ZmlsFwX1cgU0OQmrw6mjbDpoCKUHGlDb"
	discordHookProximity  = "https://discord.com/api/webhooks/1438488461144363009/XGc49LiPecDgeWyzIytsPcueb3NaigguaZc70EAh9qjtCJp6vzlIW73FQo_8rqq9yedZ"
	discordHookSpecialMil = "https://discord.com/api/webhooks/1438488588814647326/7tw61cX27pkDJKoF23r2FkxSTJ4JWEhDkRzzfHpp6x_d7YRdgd9J6SGoVUDDACUtGUXD"

	//--- API Parameters for Radius Fetching
	apiLat      = 35.740971
	apiLng      = -78.498878
	apiRadiusNM = 50

	//--- Proximity Alert Zone
	proximityRadiusNM   = 5.0
	proximityAltitudeFT = 2000.0
	earthRadiusNM       = 3440.065

	//--- Other Consts
	adsbdbAPIURL           = "https://api.adsbdb.com/v0/aircraft/" // Append {HEX}
	watchlistCSVURL        = "https://raw.githubusercontent.com/sdr-enthusiasts/plane-alert-db/main/plane-alert-db-images.csv"
	geoapifyAPIKey         = "ee4bfc4e00464753b85aa66ae3b23da6"
	radiusPollInterval     = 60 * time.Second
	nationwidePollInterval = 10 * time.Minute
	watchlistInterval      = 24 * time.Hour
)

// --- Global Variables ---
var (
	radiusAPIURL         = fmt.Sprintf("https://api.adsb.lol/v2/point/%.6f/%.6f/%d", apiLat, apiLng, apiRadiusNM)
	specialAircraftTypes = []string{"B52", "B1", "B2", "U2", "C5", "HRON", "P8"}
)

// --- Structs for ADSB.lol API (Sightings) ---
type ADSBResponse struct {
	Aircraft []Aircraft `json:"ac"`
}
type Aircraft struct {
	Hex     string  `json:"hex"`
	Flight  string  `json:"flight"`
	NNumber string  `json:"r"`
	Type    string  `json:"t"`
	Squawk  string  `json:"squawk"`
	Mil     bool    `json:"mil"`
	AltBaro any     `json:"alt_baro"`
	GS      float64 `json:"gs"`

	Lat any `json:"lat"` // For /v2/point API
	Lon any `json:"lon"` // For /v2/point API

	LastPos struct { // For /v2/type API
		Lat any `json:"lat"`
		Lon any `json:"lon"`
	} `json:"lastPosition"`
}
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
		// Path 1: Nested (for commercial)
		Aircraft struct {
			Type         string `json:"type"`
			Registration string `json:"registration"`
			Owner        string `json:"registered_owner"`
			AirlineFlag  string `json:"registered_owner_operator_flag_code"`
			ThumbnailURL string `json:"url_photo_thumbnail"`
			FullImageURL string `json:"url_photo"`
		} `json:"aircraft"`

		Type_flat         string `json:"type"`
		Registration_flat string `json:"registration"`
		Owner_flat        string `json:"owner"`
	} `json:"response"`
}
type WatchlistEntry struct {
	ICAO         string
	Registration string
	Note         string
	PlaneType    string
}
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
	Image       Image     `json:"image,omitempty"` // Using large image
	Thumbnail   Thumbnail `json:"thumbnail,omitempty"`
}
type Image struct {
	URL string `json:"url"`
}
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
type RadiusAircraftState struct {
	LastSquawk       string
	MilAlerted       bool
	WatchlistAlerted bool
	ProximityAlerted bool
	LastSeen         time.Time
}

var globalRadiusState = make(map[string]RadiusAircraftState)

// --- State for the worldwide poller (stores last alert time)
var globalNationwideState = make(map[string]time.Time)
var nationwideStateMutex = &sync.Mutex{}

// --- State for the watchlist
var (
	globalWatchlist = make(map[string]WatchlistEntry)
	watchlistMutex  = &sync.RWMutex{}
)

// --- Main Application ---
func main() {

	// Start the three main background tasks
	go manageWatchlist()    // Runs every 24 hours
	go mainRadiusLoop()     // Runs every 60 seconds
	go mainNationwideLoop() // Runs every 10 minutes

	// This is a simple way to keep the app alive
	select {}
}

// --- This is grabbing the secret watchlist from Github and holding it in memory.
func manageWatchlist() {
	ticker := time.NewTicker(watchlistInterval)
	defer ticker.Stop()
	loadWatchlistFromCSV := func() {
		////fmt.Println("[WL] Refreshing aircraft watchlist from GitHub...")
		resp, err := http.Get(watchlistCSVURL)
		if err != nil {
			//fmt.Printf("[WL] Error fetching watchlist CSV: %v\n", err)
			return
		}
		defer resp.Body.Close()

		reader := csv.NewReader(resp.Body)
		records, err := reader.ReadAll()
		if err != nil {
			//fmt.Printf("[WL] Error parsing watchlist CSV: %v\n", err)
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
		////fmt.Printf("[WL] Successfully loaded %d aircraft into watchlist.\n", len(globalWatchlist))
	}

	loadWatchlistFromCSV()
	for range ticker.C {
		loadWatchlistFromCSV()
	}
}

// --- Main 50nm Radius Poller (Watchlist & Proximity) ---
func mainRadiusLoop() {
	ticker := time.NewTicker(radiusPollInterval)
	defer ticker.Stop()

	for {
		////fmt.Println("[RD] Fetching new aircraft data (50nm)...")
		resp, err := http.Get(radiusAPIURL)
		if err != nil {
			//fmt.Printf("[RD] Error fetching ADSB data: %v\n", err)
			time.Sleep(radiusPollInterval) // Wait before retrying
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			//fmt.Printf("[RD] ADSB API returned non-200 status: %s\n", resp.Status)
			time.Sleep(radiusPollInterval)
			continue
		}

		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			//fmt.Printf("[RD] Error reading response body: %v\n", err)
			time.Sleep(radiusPollInterval)
			continue
		}

		var data ADSBResponse
		if err := json.Unmarshal(bodyBytes, &data); err != nil {
			//fmt.Printf("[RD] Error decoding JSON: %v\n", err)
			time.Sleep(radiusPollInterval)
			continue
		}

		//fmt.Printf("[RD] Processing %d aircraft...\n", len(data.Aircraft))
		for _, ac := range data.Aircraft {
			processRadiusAlerts(ac)
		}
		cleanupRadiusState()

		//fmt.Printf("[RD] Waiting for next poll in %v\n", radiusPollInterval)
		<-ticker.C
	}
}

// --- Main Special Military
func mainNationwideLoop() {
	ticker := time.NewTicker(nationwidePollInterval)
	defer ticker.Stop()

	for {
		//fmt.Println("[SM] Fetching nationwide aircraft...")

		for _, acType := range specialAircraftTypes {
			//fmt.Printf("[SM] Checking for type: %s\n", acType)
			apiURL := fmt.Sprintf("https://api.adsb.lol/v2/type/%s", acType)
			//fmt.Println(apiURL)

			resp, err := http.Get(apiURL)
			if err != nil {
				//fmt.Printf("[SM] Error fetching type %s: %v\n", acType, err)
				continue // Try next type
			}
			defer resp.Body.Close()

			var data ADSBResponse
			if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
				//fmt.Printf("[SM] Error decoding type %s: %v\n", acType, err)
				continue
			}

			if len(data.Aircraft) > 0 {
				//fmt.Printf("[SM] Found %d aircraft of type %s\n", len(data.Aircraft), acType)
			}

			for _, ac := range data.Aircraft {
				nationwideStateMutex.Lock()
				lastAlertTime, seen := globalNationwideState[ac.Hex]
				nationwideStateMutex.Unlock()

				// Only alert if we've never seen it, or last alert was > 24 hours ago
				if !seen || time.Since(lastAlertTime) > (24*time.Hour) {
					//fmt.Printf("[SM] NEW AIRCRAFT: %s (%s)\n", acType, ac.Hex)

					details, err := getAircraftDetails(ac.Hex)
					if err != nil {
						//fmt.Printf("[SM] Error getting details for %s: %v\n", ac.Hex, err)
					}

					// Send to Channel 3
					sendDiscordAlert(discordHookSpecialMil, ac, details, "special_military", nil)

					nationwideStateMutex.Lock()
					globalNationwideState[ac.Hex] = time.Now()
					nationwideStateMutex.Unlock()
				}
			}
			time.Sleep(5 * time.Second) // Small delay between API calls
		}

		//fmt.Printf("[SM] Waiting for next poll in %v\n", nationwidePollInterval)
		<-ticker.C
	}
}

// --- Helper Functions ---

// --- UPDATED: Map Generator Helper (using Geoapify) ---
func generateMapURL(lat, lon float64) string {

	zoomLevel := 8

	return fmt.Sprintf(
		"https://maps.geoapify.com/v1/staticmap?style=osm-carto&width=500&height=300&center=lonlat:%.6f,%.6f&zoom=%d&marker=lonlat:%.6f,%.6f;type:awesome;color:red&apiKey=%s",
		lon, lat, // Center of map (lon, lat)
		zoomLevel, // Zoom level (smaller = higher up)
		lon, lat,  // Location of pin (lon, lat)
		geoapifyAPIKey,
	)
}

// --- Math. Look away.
func haversine(lat1, lon1, lat2, lon2 float64) float64 {
	radLat1, radLon1 := lat1*math.Pi/180, lon1*math.Pi/180
	radLat2, radLon2 := lat2*math.Pi/180, lon2*math.Pi/180
	dLon, dLat := radLon2-radLon1, radLat2-radLat1
	a := math.Pow(math.Sin(dLat/2), 2) + math.Cos(radLat1)*math.Cos(radLat2)*math.Pow(math.Sin(dLon/2), 2)
	c := 2 * math.Asin(math.Sqrt(a))
	return c * earthRadiusNM
}

// --- Core Logic for Radius Poller ---
func processRadiusAlerts(ac Aircraft) {
	hex := ac.Hex
	squawk := ac.Squawk
	currentState, seen := globalRadiusState[hex]
	isEmergency := (squawk == "7700" || squawk == "7600" || squawk == "7500")
	lat, lon, hasCoords := getActualCoords(ac)

	// --- Trigger 1: Watchlist Hit (Channel 1) ---
	watchlistMutex.RLock()
	entry, onWatchlist := globalWatchlist[hex]
	watchlistMutex.RUnlock()

	if onWatchlist {
		if !seen || !currentState.WatchlistAlerted {
			//fmt.Printf("[Radius] !!! WATCHLIST DETECTED: %s (Note: %s)\n", hex, entry.Note)
			details, _ := getAircraftDetails(hex)
			sendDiscordAlert(discordHookWatchlist, ac, details, "watchlist", &entry)
			currentState.WatchlistAlerted = true
		}
		currentState.LastSquawk = squawk
		currentState.LastSeen = time.Now()
		globalRadiusState[hex] = currentState
		return
	}

	// --- Trigger 2: Emergency Squawk (Channel 1) ---
	if isEmergency {
		if !seen || currentState.LastSquawk != squawk {
			//fmt.Printf("[Radius] !!! EMERGENCY DETECTED: %s squawking %s\n", hex, squawk)
			details, _ := getAircraftDetails(hex)
			sendDiscordAlert(discordHookWatchlist, ac, details, "emergency", nil)
		}
		currentState.LastSquawk = squawk
		currentState.LastSeen = time.Now()
		globalRadiusState[hex] = currentState
		return
	}

	// --- Trigger 3: Military Aircraft (Channel 1) ---
	if ac.Mil {
		if !seen || !currentState.MilAlerted {
			//fmt.Printf("[Radius] !!! MILITARY DETECTED: %s\n", hex)
			details, _ := getAircraftDetails(hex)
			sendDiscordAlert(discordHookWatchlist, ac, details, "military", nil)
			currentState.MilAlerted = true
		}
		currentState.LastSquawk = squawk
		currentState.LastSeen = time.Now()
		globalRadiusState[hex] = currentState
		return
	}

	// --- Trigger 4: Proximity "Overhead" Alert (Channel 2) ---
	// Only run this check if our helper function found coordinates
	if hasCoords {
		// Use the confirmed coordinates from the helper
		distanceNM := haversine(apiLat, apiLng, lat, lon)

		if distanceNM <= proximityRadiusNM {
			altStr := formatAltitudeString(ac.AltBaro)
			altitudeFT, err := strconv.ParseFloat(altStr, 64)

			if err == nil && altitudeFT > 0 && altitudeFT <= proximityAltitudeFT {
				if !seen || !currentState.ProximityAlerted {
					//fmt.Printf("[Radius] !!! PROXIMITY DETECTED: %s (%.1f nm, %.0f ft)\n", ac.Hex, distanceNM, altitudeFT)
					details, _ := getAircraftDetails(hex)
					sendDiscordAlert(discordHookProximity, ac, details, "proximity", nil)
					currentState.ProximityAlerted = true
				}
			} else {
				currentState.ProximityAlerted = false
			}
		} else {
			currentState.ProximityAlerted = false
		}
	} else {
		// No coords, so can't be a proximity alert
		currentState.ProximityAlerted = false
	}

	currentState.LastSquawk = squawk
	currentState.LastSeen = time.Now()
	globalRadiusState[hex] = currentState
}
func cleanupRadiusState() {
	cutoff := time.Now().Add(-30 * time.Minute)
	removedCount := 0
	keysToDelete := []string{}
	for hex, state := range globalRadiusState {
		if state.LastSeen.IsZero() {
			globalRadiusState[hex] = RadiusAircraftState{LastSeen: time.Now()}
		} else if state.LastSeen.Before(cutoff) {
			keysToDelete = append(keysToDelete, hex)
		}
	}
	for _, hex := range keysToDelete {
		delete(globalRadiusState, hex)
		removedCount++
	}
	if removedCount > 0 {
		//fmt.Printf("[Radius] State cleanup complete. Removed %d old aircraft. Tracking %d.\n", removedCount, len(globalRadiusState))
	}
}

// --- On-Demand Enrichment (No-DB) ---
// --- On-Demand Enrichment (No-DB) ---
func getAircraftDetails(hex string) (AircraftDetail, error) {
	var detail AircraftDetail
	//fmt.Printf("[EN] API FETCH: Fetching details for %s from adsbdb.com\n", hex)
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

	// --- UPDATED: Multi-path mapping logic ---
	detail.Hex = hex
	if apiResponse.Response.Aircraft.Registration != "" {
		// --- Path 1: Commercial (nested) ---
		detail.Registration = apiResponse.Response.Aircraft.Registration
		detail.AircraftType = apiResponse.Response.Aircraft.Type
		detail.Owner = apiResponse.Response.Aircraft.Owner
		detail.ThumbnailURL = apiResponse.Response.Aircraft.ThumbnailURL
		detail.FullImageURL = apiResponse.Response.Aircraft.FullImageURL
		if apiResponse.Response.Aircraft.AirlineFlag != "" {
			detail.Airline = apiResponse.Response.Aircraft.AirlineFlag
		} else {
			detail.Airline = apiResponse.Response.Aircraft.Owner
		}
	} else if apiResponse.Response.Registration_flat != "" {
		// --- Path 2: Military (flat) ---
		detail.Registration = apiResponse.Response.Registration_flat
		detail.AircraftType = apiResponse.Response.Type_flat
		detail.Owner = apiResponse.Response.Owner_flat
		// (No images/airline for this path, so they remain "")
	}
	// --- END UPDATED LOGIC ---

	return detail, nil
}

func sendDiscordAlert(webhookURL string, ac Aircraft, details AircraftDetail, alertType string, entry *WatchlistEntry) {
	// --- UPDATED: Call the new coord helper ---
	// This function now correctly finds coords from either ac.Lat OR ac.LastPos.Lat
	lat, lon, hasCoords := getActualCoords(ac)
	// ---

	if webhookURL == "" || webhookURL == "https://discord.com/api/webhooks/..." {
		//fmt.Printf("[Discord] Webhook for alert type '%s' is not set. Skipping.\n", alertType)
		return
	}

	var title, description string
	var color int
	altStr := formatAltitudeString(ac.AltBaro)

	switch alertType {
	case "watchlist":
		title = "⭐️ Watchlist Alert (50nm)"
		description = fmt.Sprintf("**Note:** %s", entry.Note)
		color = 16776960 // Yellow
	case "emergency":
		title = fmt.Sprintf("🔴 EMERGENCY: SQUAWK %s", ac.Squawk)
		color = 16711680 // Red
	case "military":
		title = "✈️ Military Aircraft (50nm)"
		color = 3447003 // Blue
	case "proximity":
		title = "📡 Proximity Alert"
		description = fmt.Sprintf("**Aircraft is at %s ft within 5nm**", altStr)
		color = 16753920 // Orange
	case "special_military":
		title = fmt.Sprintf("🌎 Special Military Flight: %s", ac.Type)
		color = 11290111 // Purple
	}

	if details.FullImageURL != "" && alertType != "proximity" {
		description = fmt.Sprintf("[View Full Image](%s)\n%s", details.FullImageURL, description)
	}

	var fields []Field
	finalType := details.AircraftType
	if finalType == "" {
		if ac.Type != "" {
			finalType = ac.Type
		} else {
			finalType = "Unknown"
		}
	}

	if alertType == "special_military" {
		fields = []Field{
			{Name: "Callsign", Value: fmt.Sprintf("`%s`", ac.Flight), Inline: true},
			{Name: "ICAO Hex", Value: fmt.Sprintf("`%s`", ac.Hex), Inline: true},
			{Name: "Squawk", Value: fmt.Sprintf("`%s`", ac.Squawk), Inline: true},
			{Name: "Aircraft Type", Value: fmt.Sprintf("`%s`", finalType), Inline: true},
			{Name: "Altitude", Value: fmt.Sprintf("%s ft", altStr), Inline: true},
			{Name: "Speed", Value: fmt.Sprintf("%.1f kts", ac.GS), Inline: true},
		}
	} else {
		fields = []Field{
			{Name: "Callsign", Value: fmt.Sprintf("`%s`", ac.Flight), Inline: true},
			{Name: "ICAO Hex", Value: fmt.Sprintf("`%s`", ac.Hex), Inline: true},
			{Name: "Squawk", Value: fmt.Sprintf("`%s`", ac.Squawk), Inline: true},
			{Name: "Registration", Value: fmt.Sprintf("`%s`", details.Registration), Inline: true},
			{Name: "Aircraft Type", Value: fmt.Sprintf("`%s`", finalType), Inline: true},
			{Name: "Altitude", Value: fmt.Sprintf("%s ft", altStr), Inline: true},
			{Name: "Speed", Value: fmt.Sprintf("%.1f kts", ac.GS), Inline: true},
			{Name: "Owner", Value: details.Owner, Inline: false},
			{Name: "Airline", Value: details.Airline, Inline: false},
		}
	}

	embed := Embed{
		Title:       title,
		Description: description,
		Color:       color,
		URL:         fmt.Sprintf("https://globe.adsb.lol/?icao=%s", ac.Hex),
		Fields:      fields,
		Footer:      Footer{Text: "ADSB.lol Alerter"},
	}

	// --- UPDATED IMAGE/THUMBNAIL LOGIC ---
	// Only add the map if our helper function found coords
	if hasCoords {
		embed.Image = Image{URL: generateMapURL(lat, lon)}
	}

	if details.ThumbnailURL != "" {
		embed.Thumbnail = Thumbnail{URL: details.ThumbnailURL}
	}
	// --- END UPDATED LOGIC ---

	payload, _ := json.Marshal(DiscordWebhook{Embeds: []Embed{embed}})
	resp, err := http.Post(webhookURL, "application/json", bytes.NewBuffer(payload))
	if err != nil {
		//fmt.Printf("[Discord] Error sending alert: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		//fmt.Printf("[Discord] API returned non-2xx status: %s\n", resp.Status)
	} else {
		//fmt.Printf("[Discord] Successfully sent alert for %s (Type: %s)\n", ac.Hex, alertType)
	}
}

// --- Here are some format helpers
func getActualCoords(ac Aircraft) (lat float64, lon float64, hasCoords bool) {
	// 1. Try to parse top-level fields (from /v2/point)
	lat = parseFloat(ac.Lat)
	lon = parseFloat(ac.Lon)

	if lat != 0 && lon != 0 {
		// We found valid top-level coords.
		return lat, lon, true
	}

	// 2. Top-level failed. Try 'lastPosition' fields (from /v2/type)
	lat = parseFloat(ac.LastPos.Lat)
	lon = parseFloat(ac.LastPos.Lon)

	if lat != 0 && lon != 0 {
		// We found valid 'lastPosition' coords.
		return lat, lon, true
	}

	// 3. We tried both and failed.
	return 0, 0, false
}
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
func parseFloat(val any) float64 {
	var f float64
	var err error

	switch v := val.(type) {
	case float64:
		f = v
	case string:
		f, err = strconv.ParseFloat(v, 64)
		if err != nil {
			f = 0.0 // On parse error, return 0.0
		}
	default:
		f = 0.0 // Unsupported type
	}
	return f
}

-- Connect to the specified database


--
-- Table 1: aircraft_details (The Cache)
-- This table stores the rich details from adsbdb.com.
-- The 'hex' column is the PRIMARY KEY to prevent duplicates.
--
CREATE TABLE IF NOT EXISTS aircraft_details (
    hex TEXT PRIMARY KEY,
    registration TEXT,
    airline TEXT,
    owner TEXT,
    aircraft_type TEXT,
    note TEXT,
    last_fetched_at TIMESTAMPTZ DEFAULT NOW()
);

COMMENT ON TABLE aircraft_details IS 'Cache for rich aircraft data from adsbdb.com.';
COMMENT ON COLUMN aircraft_details.hex IS 'ICAO hex code (Primary Key).';
COMMENT ON COLUMN aircraft_details.last_fetched_at IS 'When this row was last fetched/updated from the API.';


--
-- Table 2: aircraft_sightings (The Log)
-- This table stores every single aircraft sighting processed by the script.
--
CREATE TABLE IF NOT EXISTS aircraft_sightings (
    id SERIAL PRIMARY KEY,
    seen_at TIMESTAMPTZ DEFAULT NOW(),
    hex TEXT,
    flight TEXT,
    n_number TEXT,
    squawk TEXT,
    mil BOOLEAN,
    alt_baro TEXT,
    gs NUMERIC(6, 1),
    lat NUMERIC(9, 6),
    lon NUMERIC(9, 6)
);

COMMENT ON TABLE aircraft_sightings IS 'Log of all aircraft sightings processed by the alerter.';
COMMENT ON COLUMN aircraft_sightings.hex IS 'ICAO hex code of the sighted aircraft.';
COMMENT ON COLUMN aircraft_sightings.alt_baro IS 'Altitude (can be "ground" or a number).';


--
-- Index: idx_sightings_hex
-- Creates an index on the 'hex' column in the sightings table.
-- This will make it much faster to search for all sightings of a specific aircraft.
--
CREATE INDEX IF NOT EXISTS idx_sightings_hex ON aircraft_sightings (hex);

--
-- Grant Permissions (Optional but Recommended)
-- Replace 'YOUR_USER' with the username your Go app uses.
--
GRANT SELECT, INSERT, UPDATE, DELETE ON aircraft_details TO avnadmin;
GRANT SELECT, INSERT ON aircraft_sightings TO avnadmin;
GRANT USAGE, SELECT ON SEQUENCE aircraft_sightings_id_seq TO avnadmin;

--
-- End of script
--
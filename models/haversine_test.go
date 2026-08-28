package models

import (
	"math"
	"testing"
	"time"

	geo "github.com/kellydunn/golang-geo"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/utils"
)

// lyon and paris are the reference pair from the README.
var (
	lyon  = coordinate{lat: 45.7597, lng: 4.8422}
	paris = coordinate{lat: 48.8567, lng: 2.3508}
)

func TestHaversineKm(t *testing.T) {
	got := haversineKm(lyon, paris)
	if math.Abs(got-392.2172595594006) > 1e-9 {
		t.Fatalf("haversineKm(lyon, paris) = %v, want 392.2172595594006", got)
	}

	if got := haversineKm(lyon, lyon); got != 0 {
		t.Fatalf("distance from a point to itself = %v, want 0", got)
	}
}

func TestCalculateDistances(t *testing.T) {
	result := calculateDistances(lyon, paris)

	for _, tc := range []struct {
		key  string
		want float64
	}{
		{"distance_km", 392.2172595594006},
		{"distance_miles", 243.71250609539814},
		{"distance_nautical_miles", 211.78037755311516},
	} {
		got, ok := result[tc.key].(float64)
		if !ok {
			t.Fatalf("%s missing or not a float: %v", tc.key, result[tc.key])
		}
		if math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("%s = %v, want %v", tc.key, got, tc.want)
		}
	}

	location1, ok := result["location_1"].(map[string]interface{})
	if !ok {
		t.Fatalf("location_1 missing or not an object: %v", result["location_1"])
	}
	if location1["latitude"] != lyon.lat || location1["longitude"] != lyon.lng {
		t.Errorf("location_1 = %v, want %v", location1, lyon)
	}
}

func TestParseDuration(t *testing.T) {
	valid := map[string]time.Duration{
		"1d":    24 * time.Hour,
		"12h":   12 * time.Hour,
		"10m":   10 * time.Minute,
		"30s":   30 * time.Second,
		"100ms": 100 * time.Millisecond,
	}
	for input, want := range valid {
		got, err := parseDuration(input)
		if err != nil {
			t.Fatalf("parseDuration(%q) returned error: %v", input, err)
		}
		if got != want {
			t.Errorf("parseDuration(%q) = %v, want %v", input, got, want)
		}
	}

	for _, input := range []string{"", "5", "m", "1w", "1.5h", "-1h", "10 m"} {
		if _, err := parseDuration(input); err == nil {
			t.Errorf("parseDuration(%q) succeeded, want an error", input)
		}
	}
}

func TestValueAtPath(t *testing.T) {
	readings := map[string]interface{}{
		"position": geo.NewPoint(45.7597, 4.8422),
		"loc": map[string]interface{}{
			"latitude":  "48.8567",
			"longitude": map[string]interface{}{"value": 2.3508},
		},
		"timestamp": "2023-12-01T10:30:00Z",
	}

	for _, tc := range []struct {
		path []string
		want float64
	}{
		{[]string{"position", "lat"}, 45.7597},
		{[]string{"position", "longitude"}, 4.8422},
		{[]string{"loc", "latitude"}, 48.8567},
		{[]string{"loc", "longitude"}, 2.3508},
	} {
		got, err := floatAtPath(readings, tc.path)
		if err != nil {
			t.Fatalf("floatAtPath(%v) returned error: %v", tc.path, err)
		}
		if got != tc.want {
			t.Errorf("floatAtPath(%v) = %v, want %v", tc.path, got, tc.want)
		}
	}

	for _, path := range [][]string{{"missing"}, {"loc", "altitude"}, {"position", "elevation"}, {"timestamp", "lat"}} {
		if _, err := floatAtPath(readings, path); err == nil {
			t.Errorf("floatAtPath(%v) succeeded, want an error", path)
		}
	}
}

func TestToTime(t *testing.T) {
	want := time.Date(2023, 12, 1, 10, 30, 0, 0, time.UTC)
	for _, input := range []string{
		"2023-12-01T10:30:00Z",
		"2023-12-01T10:30:00+00:00",
		"2023-12-01T10:30:00.000Z",
		// Spellings Python's fromisoformat accepts. The space separator is
		// what str(datetime.now(timezone.utc)) produces, so a Python source
		// sensor emits it by default.
		"2023-12-01 10:30:00+00:00",
		"2023-12-01 10:30:00.000000+00:00",
		"2023-12-01T10:30:00+0000",
		"2023-12-01T10:30:00-00",
		"20231201T103000Z",
		"20231201T103000+0000",
		"2023-12-01T10:30Z",
	} {
		got, err := toTime(input)
		if err != nil {
			t.Fatalf("toTime(%q) returned error: %v", input, err)
		}
		if !got.Equal(want) {
			t.Errorf("toTime(%q) = %v, want %v", input, got, want)
		}
	}

	// A zoneless timestamp is read in the local zone, so the same wall clock
	// time is a different instant depending on where the machine runs.
	local, err := toTime("2023-12-01 10:30:00")
	if err != nil {
		t.Fatalf("toTime on a zoneless timestamp returned error: %v", err)
	}
	if !local.Equal(time.Date(2023, 12, 1, 10, 30, 0, 0, time.Local)) {
		t.Errorf("toTime on a zoneless timestamp = %v, want it read in the local zone", local)
	}

	for _, input := range []string{"not a timestamp", "", "2023-13-45T99:99:99Z"} {
		if _, err := toTime(input); err == nil {
			t.Errorf("toTime(%q) succeeded, want an error", input)
		}
	}
}

func TestReadingValid(t *testing.T) {
	s := &haversineSensor{logger: logging.NewTestLogger(t)}
	recent := time.Now().Add(-time.Minute).Format(time.RFC3339)
	stale := time.Now().Add(-2 * time.Hour).Format(time.RFC3339)

	src := &source{updatedPath: []string{"updated"}, expire: 30 * time.Minute}

	if valid, err := s.readingValid(map[string]interface{}{"updated": recent}, src); err != nil || !valid {
		t.Errorf("a reading one minute old was rejected: valid=%v err=%v", valid, err)
	}
	if valid, err := s.readingValid(map[string]interface{}{"updated": stale}, src); err != nil || valid {
		t.Errorf("a reading two hours old was reported as valid: valid=%v err=%v", valid, err)
	}

	// A timestamp that cannot be read at all is reported separately from an
	// expired one: the caller says so rather than blaming the reading's age.
	for _, readings := range []map[string]interface{}{
		{},
		{"updated": "not a timestamp"},
		{"updated": []string{"wrong type"}},
	} {
		valid, err := s.readingValid(readings, src)
		if err == nil {
			t.Errorf("readingValid(%v) returned no error, want an unreadable timestamp reported", readings)
		}
		if valid {
			t.Errorf("readingValid(%v) = true, want false", readings)
		}
	}

	// Without both an updated path and an expire duration, age is not checked.
	if valid, err := s.readingValid(map[string]interface{}{"updated": stale}, &source{}); err != nil || !valid {
		t.Errorf("an unconfigured expiry rejected a stale reading: valid=%v err=%v", valid, err)
	}
}

func TestDoCommandRejectsIncompleteLocations(t *testing.T) {
	s := &haversineSensor{logger: logging.NewTestLogger(t)}
	valid := map[string]interface{}{"latitude": 45.7597, "longitude": 4.8422}

	result, err := s.DoCommand(t.Context(), map[string]interface{}{
		"location_1": valid,
		"location_2": map[string]interface{}{"latitude": 48.8567, "longitude": 2.3508},
	})
	if err != nil {
		t.Fatalf("DoCommand with two locations returned error: %v", err)
	}
	if _, ok := result["distance_km"]; !ok {
		t.Fatalf("DoCommand result is missing distance_km: %v", result)
	}

	for _, cmd := range []map[string]interface{}{
		{},
		{"location_1": valid},
		{"location_1": valid, "location_2": map[string]interface{}{"latitude": 48.8567}},
		{"location_1": valid, "location_2": "48.8567,2.3508"},
		{"location_1": valid, "location_2": map[string]interface{}{"latitude": 148.8567, "longitude": 2.3508}},
		{"location_1": valid, "location_2": map[string]interface{}{"latitude": 48.8567, "longitude": -200.0}},
	} {
		if _, err := s.DoCommand(t.Context(), cmd); err == nil {
			t.Errorf("DoCommand(%v) succeeded, want an error", cmd)
		}
	}
}

func TestValidate(t *testing.T) {
	complete := &SensorConfig{Name: "gps1", Latitude: "position.lat", Longitude: "position.lng"}

	_, optional, err := (&Config{Sensor1: complete, Sensor2: &SensorConfig{
		Name: "phone", Latitude: "loc.latitude", Longitude: "loc.longitude", Updated: "last_updated", Expire: "1h",
	}}).Validate("")
	if err != nil {
		t.Fatalf("Validate on a complete config returned error: %v", err)
	}
	if len(optional) != 2 || optional[0] != "gps1" || optional[1] != "phone" {
		t.Errorf("optional dependencies = %v, want [gps1 phone]", optional)
	}

	if _, _, err := (&Config{}).Validate(""); err != nil {
		t.Errorf("Validate on an empty config returned error: %v", err)
	}

	for _, cfg := range []*Config{
		{Sensor1: &SensorConfig{Latitude: "a", Longitude: "b"}},
		{Sensor1: &SensorConfig{Name: "gps1", Longitude: "b"}},
		{Sensor2: &SensorConfig{Name: "gps1", Latitude: "a"}},
		{Sensor1: complete, Sensor2: &SensorConfig{Name: "p", Latitude: "a", Longitude: "b", Updated: "t", Expire: "1w"}},
		// Half a staleness config never checks the age, so it is rejected
		// rather than silently reporting stale coordinates as fresh.
		{Sensor1: &SensorConfig{Name: "gps1", Latitude: "a", Longitude: "b", Expire: "10m"}},
		{Sensor1: &SensorConfig{Name: "gps1", Latitude: "a", Longitude: "b", Updated: "t"}},
	} {
		if _, _, err := cfg.Validate(""); err == nil {
			t.Errorf("Validate(%v) succeeded, want an error", cfg)
		}
	}
}

func TestCoordinateRejectsNonFiniteValues(t *testing.T) {
	// A movement sensor with no position fix reports NaN for both fields. Every
	// comparison against NaN is false, so a plain range check lets it through
	// and the reported distances all become NaN.
	for _, c := range []coordinate{
		{lat: math.NaN(), lng: math.NaN()},
		{lat: math.NaN(), lng: 4.8422},
		{lat: 45.7597, lng: math.NaN()},
		{lat: math.Inf(1), lng: 4.8422},
		{lat: 45.7597, lng: math.Inf(-1)},
	} {
		if err := c.validate(); err == nil {
			t.Errorf("coordinate{%v, %v}.validate() succeeded, want an error", c.lat, c.lng)
		}
	}
}

// embedded reproduces the shape that makes reflect.Value.FieldByNameFunc panic:
// the named field is promoted through an embedded pointer that is nil.
type embeddedPosition struct {
	Latitude float64
}

type promotedReading struct {
	*embeddedPosition
	Name string
}

func TestFieldValueOnNilEmbeddedPointer(t *testing.T) {
	reading := promotedReading{Name: "gps1"}

	// The panic this guards against would take the module process down rather
	// than report an unusable path.
	if _, err := fieldValue(reading, "latitude"); err == nil {
		t.Error("fieldValue through a nil embedded pointer succeeded, want an error")
	}

	// A field promoted through a populated embedded pointer still resolves.
	reading.embeddedPosition = &embeddedPosition{Latitude: 45.7597}
	got, err := fieldValue(reading, "latitude")
	if err != nil {
		t.Fatalf("fieldValue on a promoted field returned error: %v", err)
	}
	if got != 45.7597 {
		t.Errorf("fieldValue on a promoted field = %v, want 45.7597", got)
	}
}

func TestAttributeConversion(t *testing.T) {
	attrs := utils.AttributeMap{
		"sensor_1": map[string]interface{}{
			"name": "gps1", "latitude": "position.lat", "longitude": "position.lng",
			"updated": "timestamp", "expire": "5m",
		},
		"sensor_2": map[string]interface{}{
			"name": "phone_data", "latitude": "loc.latitude", "longitude": "loc.longitude",
		},
	}
	cfg, err := resource.TransformAttributeMap[*Config](attrs)
	if err != nil {
		t.Fatalf("TransformAttributeMap returned error: %v", err)
	}
	if cfg.Sensor1 == nil || cfg.Sensor1.Name != "gps1" || cfg.Sensor1.Latitude != "position.lat" ||
		cfg.Sensor1.Longitude != "position.lng" || cfg.Sensor1.Updated != "timestamp" || cfg.Sensor1.Expire != "5m" {
		t.Errorf("sensor_1 decoded as %+v", cfg.Sensor1)
	}
	if cfg.Sensor2 == nil || cfg.Sensor2.Name != "phone_data" || cfg.Sensor2.Expire != "" {
		t.Errorf("sensor_2 decoded as %+v", cfg.Sensor2)
	}
}

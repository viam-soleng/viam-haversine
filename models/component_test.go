package models

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	geo "github.com/kellydunn/golang-geo"
	"go.viam.com/rdk/components/movementsensor"
	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/testutils/inject"
)

// newTestSensor returns an injected plain sensor reporting a fixed nested reading.
func newTestSensor(name string, readings map[string]interface{}) *inject.Sensor {
	s := inject.NewSensor(name)
	s.ReadingsFunc = func(ctx context.Context, extra map[string]interface{}) (map[string]interface{}, error) {
		return readings, nil
	}
	return s
}

// newTestMovementSensor returns an injected movement sensor whose Readings look
// like a real one's: the position arrives as a *geo.Point.
func newTestMovementSensor(name string, point *geo.Point) *inject.MovementSensor {
	ms := inject.NewMovementSensor(name)
	ms.ReadingsFunc = func(ctx context.Context, extra map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"position": point, "altitude": 0.0}, nil
	}
	return ms
}

func newTestComponent(t *testing.T, cfg *Config, deps resource.Dependencies) sensor.Sensor {
	t.Helper()
	conf := resource.Config{
		Name:                "haversine",
		API:                 sensor.API,
		Model:               Model,
		ConvertedAttributes: cfg,
	}
	s, err := newHaversine(t.Context(), deps, conf, logging.NewTestLogger(t))
	if err != nil {
		t.Fatalf("newHaversine returned error: %v", err)
	}
	t.Cleanup(func() { s.Close(context.Background()) })
	return s
}

// TestReadingsDoesNotForwardExtra pins that the sources are read with no extra
// of their own. viam-server's data manager passes {"fromDataManagement": true}
// when it captures this component, and forwarding that would change the
// behaviour of any source sensor that branches on it.
func TestReadingsDoesNotForwardExtra(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]map[string]interface{}{}
	record := func(name string, readings map[string]interface{}) *inject.Sensor {
		s := inject.NewSensor(name)
		s.ReadingsFunc = func(ctx context.Context, extra map[string]interface{}) (map[string]interface{}, error) {
			mu.Lock()
			defer mu.Unlock()
			seen[name] = extra
			return readings, nil
		}
		return s
	}

	deps := resource.Dependencies{
		sensor.Named("a"): record("a", map[string]interface{}{"latitude": lyon.lat, "longitude": lyon.lng}),
		sensor.Named("b"): record("b", map[string]interface{}{"latitude": paris.lat, "longitude": paris.lng}),
	}
	cfg := &Config{
		Sensor1: &SensorConfig{Name: "a", Latitude: "latitude", Longitude: "longitude"},
		Sensor2: &SensorConfig{Name: "b", Latitude: "latitude", Longitude: "longitude"},
	}
	s := newTestComponent(t, cfg, deps)

	if _, err := s.Readings(t.Context(), map[string]interface{}{"fromDataManagement": true}); err != nil {
		t.Fatalf("Readings returned error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, name := range []string{"a", "b"} {
		if extra, ok := seen[name]; !ok {
			t.Errorf("sensor %q was not read", name)
		} else if len(extra) != 0 {
			t.Errorf("sensor %q was read with extra = %v, want none forwarded", name, extra)
		}
	}
}

func TestReadingsAcrossSensorTypes(t *testing.T) {
	deps := resource.Dependencies{
		movementsensor.Named("gps1"): newTestMovementSensor("gps1", geo.NewPoint(lyon.lat, lyon.lng)),
		sensor.Named("phone"): newTestSensor("phone", map[string]interface{}{
			"loc": map[string]interface{}{"latitude": paris.lat, "longitude": paris.lng},
		}),
	}
	s := newTestComponent(t, &Config{
		Sensor1: &SensorConfig{Name: "gps1", Latitude: "position.lat", Longitude: "position.lng"},
		Sensor2: &SensorConfig{Name: "phone", Latitude: "loc.latitude", Longitude: "loc.longitude"},
	}, deps)

	readings, err := s.Readings(t.Context(), nil)
	if err != nil {
		t.Fatalf("Readings returned error: %v", err)
	}
	km, ok := readings["distance_km"].(float64)
	if !ok {
		t.Fatalf("distance_km missing or not a float: %v", readings["distance_km"])
	}
	if math.Abs(km-392.2172595594006) > 1e-9 {
		t.Errorf("distance_km = %v, want 392.2172595594006", km)
	}
}

func TestReadingsEmptyWithoutBothSensors(t *testing.T) {
	deps := resource.Dependencies{
		sensor.Named("phone"): newTestSensor("phone", map[string]interface{}{
			"loc": map[string]interface{}{"latitude": paris.lat, "longitude": paris.lng},
		}),
	}

	for _, tc := range []struct {
		name string
		cfg  *Config
	}{
		{"no sensors", &Config{}},
		{"one sensor", &Config{Sensor2: &SensorConfig{Name: "phone", Latitude: "loc.latitude", Longitude: "loc.longitude"}}},
		{"missing dependency", &Config{
			Sensor1: &SensorConfig{Name: "absent", Latitude: "loc.latitude", Longitude: "loc.longitude"},
			Sensor2: &SensorConfig{Name: "phone", Latitude: "loc.latitude", Longitude: "loc.longitude"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			readings, err := newTestComponent(t, tc.cfg, deps).Readings(t.Context(), nil)
			if err != nil {
				t.Fatalf("Readings returned error: %v", err)
			}
			if len(readings) != 0 {
				t.Errorf("Readings = %v, want an empty result", readings)
			}
		})
	}
}

func TestReadingsEmptyWhenExpired(t *testing.T) {
	stale := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	fresh := time.Now().UTC().Format(time.RFC3339)

	deps := resource.Dependencies{
		sensor.Named("a"): newTestSensor("a", map[string]interface{}{
			"latitude": lyon.lat, "longitude": lyon.lng, "updated": stale,
		}),
		sensor.Named("b"): newTestSensor("b", map[string]interface{}{
			"latitude": paris.lat, "longitude": paris.lng, "updated": fresh,
		}),
	}
	cfg := &Config{
		Sensor1: &SensorConfig{Name: "a", Latitude: "latitude", Longitude: "longitude", Updated: "updated", Expire: "5m"},
		Sensor2: &SensorConfig{Name: "b", Latitude: "latitude", Longitude: "longitude", Updated: "updated", Expire: "5m"},
	}

	readings, err := newTestComponent(t, cfg, deps).Readings(t.Context(), nil)
	if err != nil {
		t.Fatalf("Readings returned error: %v", err)
	}
	if len(readings) != 0 {
		t.Errorf("Readings = %v, want an empty result for an expired sensor", readings)
	}

	// The same sensors, with an expiry long enough to keep both readings valid.
	cfg.Sensor1.Expire = "2h"
	cfg.Sensor2.Expire = "2h"
	readings, err = newTestComponent(t, cfg, deps).Readings(t.Context(), nil)
	if err != nil {
		t.Fatalf("Readings returned error: %v", err)
	}
	if _, ok := readings["distance_km"]; !ok {
		t.Errorf("Readings = %v, want a distance once the readings are within the expiry", readings)
	}
}

func TestReadingsErrorOnBadPath(t *testing.T) {
	deps := resource.Dependencies{
		sensor.Named("a"): newTestSensor("a", map[string]interface{}{"latitude": lyon.lat, "longitude": lyon.lng}),
		sensor.Named("b"): newTestSensor("b", map[string]interface{}{"latitude": paris.lat, "longitude": paris.lng}),
	}
	s := newTestComponent(t, &Config{
		Sensor1: &SensorConfig{Name: "a", Latitude: "lat", Longitude: "longitude"},
		Sensor2: &SensorConfig{Name: "b", Latitude: "latitude", Longitude: "longitude"},
	}, deps)

	if _, err := s.Readings(t.Context(), nil); err == nil {
		t.Error("Readings with a bad latitude path succeeded, want an error")
	}
}

func TestRebuildPicksUpNewSensors(t *testing.T) {
	nearby := geo.NewPoint(lyon.lat+0.01, lyon.lng)
	deps := resource.Dependencies{
		sensor.Named("a"):         newTestSensor("a", map[string]interface{}{"latitude": lyon.lat, "longitude": lyon.lng}),
		sensor.Named("b"):         newTestSensor("b", map[string]interface{}{"latitude": paris.lat, "longitude": paris.lng}),
		movementsensor.Named("c"): newTestMovementSensor("c", nearby),
	}

	// The framework rebuilds the component on a config change, so a new
	// configuration means a new component.
	first := newTestComponent(t, &Config{
		Sensor1: &SensorConfig{Name: "a", Latitude: "latitude", Longitude: "longitude"},
		Sensor2: &SensorConfig{Name: "b", Latitude: "latitude", Longitude: "longitude"},
	}, deps)
	second := newTestComponent(t, &Config{
		Sensor1: &SensorConfig{Name: "a", Latitude: "latitude", Longitude: "longitude"},
		Sensor2: &SensorConfig{Name: "c", Latitude: "position.lat", Longitude: "position.lng"},
	}, deps)

	for _, tc := range []struct {
		name string
		s    sensor.Sensor
		want float64
	}{
		{"original", first, haversineKm(lyon, paris)},
		{"rebuilt", second, haversineKm(lyon, coordinate{lat: nearby.Lat(), lng: nearby.Lng()})},
	} {
		readings, err := tc.s.Readings(t.Context(), nil)
		if err != nil {
			t.Fatalf("%s: Readings returned error: %v", tc.name, err)
		}
		km, ok := readings["distance_km"].(float64)
		if !ok {
			t.Fatalf("%s: distance_km missing or not a float: %v", tc.name, readings["distance_km"])
		}
		if math.Abs(km-tc.want) > 1e-9 {
			t.Errorf("%s: distance_km = %v, want %v", tc.name, km, tc.want)
		}
	}
}

func TestClose(t *testing.T) {
	deps := resource.Dependencies{
		sensor.Named("a"): newTestSensor("a", map[string]interface{}{"latitude": lyon.lat, "longitude": lyon.lng}),
		sensor.Named("b"): newTestSensor("b", map[string]interface{}{"latitude": paris.lat, "longitude": paris.lng}),
	}
	s := newTestComponent(t, &Config{
		Sensor1: &SensorConfig{Name: "a", Latitude: "latitude", Longitude: "longitude"},
		Sensor2: &SensorConfig{Name: "b", Latitude: "latitude", Longitude: "longitude"},
	}, deps)

	if _, err := s.Readings(t.Context(), nil); err != nil {
		t.Fatalf("Readings before Close returned error: %v", err)
	}

	// Close is idempotent.
	for i := range 2 {
		if err := s.Close(t.Context()); err != nil {
			t.Fatalf("Close call %d returned error: %v", i+1, err)
		}
	}

	if _, err := s.Readings(t.Context(), nil); !errors.Is(err, errClosed) {
		t.Errorf("Readings after Close returned %v, want %v", err, errClosed)
	}
	_, err := s.DoCommand(t.Context(), map[string]interface{}{
		"location_1": map[string]interface{}{"latitude": lyon.lat, "longitude": lyon.lng},
		"location_2": map[string]interface{}{"latitude": paris.lat, "longitude": paris.lng},
	})
	if !errors.Is(err, errClosed) {
		t.Errorf("DoCommand after Close returned %v, want %v", err, errClosed)
	}
}

// TestCloseDuringReadings exercises Close racing a reading under the race detector.
func TestCloseDuringReadings(t *testing.T) {
	deps := resource.Dependencies{
		sensor.Named("a"): newTestSensor("a", map[string]interface{}{"latitude": lyon.lat, "longitude": lyon.lng}),
		sensor.Named("b"): newTestSensor("b", map[string]interface{}{"latitude": paris.lat, "longitude": paris.lng}),
	}
	s := newTestComponent(t, &Config{
		Sensor1: &SensorConfig{Name: "a", Latitude: "latitude", Longitude: "longitude"},
		Sensor2: &SensorConfig{Name: "b", Latitude: "latitude", Longitude: "longitude"},
	}, deps)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Either a distance or errClosed is correct here; a data race is not.
			if _, err := s.Readings(t.Context(), nil); err != nil && !errors.Is(err, errClosed) {
				t.Errorf("Readings returned an unexpected error: %v", err)
			}
		}()
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	wg.Wait()
}

// Package models implements the viam-soleng:haversine:haversine sensor.
//
// The component calculates the great-circle distance between two points on
// Earth, either from the readings of two configured sensors or from two
// coordinate pairs handed to DoCommand.
package models

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	geo "github.com/kellydunn/golang-geo"
	"go.viam.com/rdk/components/movementsensor"
	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
)

// Model is the resource model triple for this sensor.
var Model = resource.NewModel("viam-soleng", "haversine", "haversine")

// Unit conversions from kilometers. These are the factors used by the Python
// `haversine` package, so the Go port reports identical distances.
const (
	earthRadiusKm     = 6371.0088
	kmToMiles         = 0.621371192
	kmToNauticalMiles = 0.539956803
)

func init() {
	resource.RegisterComponent(sensor.API, Model,
		resource.Registration[sensor.Sensor, *Config]{
			Constructor: newHaversine,
		},
	)
}

// SensorConfig describes one of the two location sources. The latitude,
// longitude and updated attributes are dot-separated paths into that sensor's
// readings, e.g. "position.lat".
type SensorConfig struct {
	Name      string `json:"name"`
	Latitude  string `json:"latitude"`
	Longitude string `json:"longitude"`
	Updated   string `json:"updated,omitempty"`
	Expire    string `json:"expire,omitempty"`
}

// Config holds the component configuration. Both sensors are optional; without
// them Readings returns an empty result and only DoCommand is functional.
type Config struct {
	Sensor1 *SensorConfig `json:"sensor_1,omitempty"`
	Sensor2 *SensorConfig `json:"sensor_2,omitempty"`
}

// Validate checks each configured sensor and reports them as optional
// dependencies, so a missing sensor degrades Readings rather than failing the
// whole component.
func (cfg *Config) Validate(path string) ([]string, []string, error) {
	var optional []string

	for _, s := range []struct {
		field string
		conf  *SensorConfig
	}{
		{"sensor_1", cfg.Sensor1},
		{"sensor_2", cfg.Sensor2},
	} {
		if s.conf == nil {
			continue
		}
		if s.conf.Name == "" || s.conf.Latitude == "" || s.conf.Longitude == "" {
			return nil, nil, fmt.Errorf("%s if configured must have name, latitude, and longitude fields", s.field)
		}
		if s.conf.Expire != "" {
			if _, err := parseDuration(s.conf.Expire); err != nil {
				return nil, nil, fmt.Errorf("%s: %w", s.field, err)
			}
		}
		optional = append(optional, s.conf.Name)
	}

	return nil, optional, nil
}

// source is a configured location sensor resolved against the dependencies.
// Any resource that reports readings qualifies, which covers both Sensor and
// MovementSensor components.
type source struct {
	name        string
	kind        string
	reader      resource.Sensor
	latPath     []string
	lngPath     []string
	updatedPath []string
	expire      time.Duration
}

// errClosed is returned by any method called after the component is closed.
var errClosed = errors.New("haversine sensor is closed")

type haversineSensor struct {
	resource.Named

	logger logging.Logger

	// closed is atomic because the module framework closes a resource while it
	// is still reachable over gRPC, so Close can overlap an in-flight reading.
	closed atomic.Bool

	// Both sources are resolved during construction, before the framework
	// publishes the resource, and are never written again: the component is
	// rebuilt when its config or dependencies change.
	source1 *source
	source2 *source
}

func newHaversine(
	ctx context.Context,
	deps resource.Dependencies,
	conf resource.Config,
	logger logging.Logger,
) (sensor.Sensor, error) {
	s := &haversineSensor{
		Named:  conf.ResourceName().AsNamed(),
		logger: logger,
	}
	if err := s.configure(deps, conf); err != nil {
		return nil, err
	}
	return s, nil
}

// configure resolves both sensors from the dependencies. A configured sensor
// that is missing is logged and left unset rather than treated as an error.
func (s *haversineSensor) configure(deps resource.Dependencies, conf resource.Config) error {
	cfg, err := resource.NativeConfig[*Config](conf)
	if err != nil {
		return err
	}

	source1, err := s.newSource("sensor_1", cfg.Sensor1, deps)
	if err != nil {
		return err
	}
	source2, err := s.newSource("sensor_2", cfg.Sensor2, deps)
	if err != nil {
		return err
	}

	if source1 == nil || source2 == nil {
		s.logger.Warn("One or both sensors not configured - Readings() will return empty results. Only DoCommand() will be fully functional.")
	}

	s.source1 = source1
	s.source2 = source2
	return nil
}

func (s *haversineSensor) newSource(field string, conf *SensorConfig, deps resource.Dependencies) (*source, error) {
	if conf == nil {
		return nil, nil
	}

	reader, kind := findSensor(conf.Name, deps)
	if reader == nil {
		s.logger.Warnf("Configured %s '%s' not found in dependencies (tried both Sensor and MovementSensor)", field, conf.Name)
		return nil, nil
	}
	s.logger.Infof("Configuring %s as %s", field, kind)

	src := &source{
		name:    conf.Name,
		kind:    kind,
		reader:  reader,
		latPath: strings.Split(conf.Latitude, "."),
		lngPath: strings.Split(conf.Longitude, "."),
	}
	if conf.Updated != "" {
		src.updatedPath = strings.Split(conf.Updated, ".")
	}
	if conf.Expire != "" {
		expire, err := parseDuration(conf.Expire)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", field, err)
		}
		src.expire = expire
	}
	return src, nil
}

// findSensor looks the named component up as a sensor, then as a movement
// sensor, then by short name across every dependency. It returns the resource
// and a description of the API it was found under.
func findSensor(name string, deps resource.Dependencies) (resource.Sensor, string) {
	for _, candidate := range []struct {
		resourceName resource.Name
		kind         string
	}{
		{sensor.Named(name), "sensor"},
		{movementsensor.Named(name), "movement_sensor"},
	} {
		if res, err := deps.Lookup(candidate.resourceName); err == nil {
			if reader, ok := res.(resource.Sensor); ok {
				return reader, candidate.kind
			}
		}
	}

	// The dependency may have been resolved under some other readings-capable
	// API (a power sensor, for instance), in which case only the short name matches.
	for depName, res := range deps {
		if depName.ShortName() != name && depName.Name != name {
			continue
		}
		if reader, ok := res.(resource.Sensor); ok {
			return reader, depName.API.SubtypeName
		}
	}

	return nil, ""
}

// sources returns the two configured sensors, or an error if the component has been closed.
func (s *haversineSensor) sources() (*source, *source, error) {
	if s.closed.Load() {
		return nil, nil, errClosed
	}
	return s.source1, s.source2, nil
}

// Readings returns the distance between the two configured sensors. It returns
// an empty result when either sensor is unconfigured or its reading has expired.
func (s *haversineSensor) Readings(ctx context.Context, extra map[string]interface{}) (map[string]interface{}, error) {
	source1, source2, err := s.sources()
	if err != nil {
		return nil, err
	}

	if source1 == nil || source2 == nil {
		return map[string]interface{}{}, nil
	}

	readings1, err := source1.reader.Readings(ctx, extra)
	if err != nil {
		return nil, fmt.Errorf("error reading sensor_1 '%s': %w", source1.name, err)
	}
	readings2, err := source2.reader.Readings(ctx, extra)
	if err != nil {
		return nil, fmt.Errorf("error reading sensor_2 '%s': %w", source2.name, err)
	}

	s.logger.Debugf("Sensor 1 readings: %v", readings1)
	s.logger.Debugf("Sensor 2 readings: %v", readings2)

	if !s.readingValid(readings1, source1) {
		s.logger.Warn("Sensor 1 reading has expired")
		return map[string]interface{}{}, nil
	}
	if !s.readingValid(readings2, source2) {
		s.logger.Warn("Sensor 2 reading has expired")
		return map[string]interface{}{}, nil
	}

	point1, err := source1.point(readings1)
	if err != nil {
		s.logger.Errorf("Error extracting coordinates: %v", err)
		return nil, err
	}
	point2, err := source2.point(readings2)
	if err != nil {
		s.logger.Errorf("Error extracting coordinates: %v", err)
		return nil, err
	}

	s.logger.Debugf("Extracted coordinates - Point 1: (%v, %v), Point 2: (%v, %v)",
		point1.lat, point1.lng, point2.lat, point2.lng)
	return calculateDistances(point1, point2), nil
}

// DoCommand calculates the distance between two coordinate pairs, independent
// of any sensor configuration.
func (s *haversineSensor) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	const usage = "Both location_1 and location_2 must be provided in the format: {'latitude': float, 'longitude': float}"

	if _, _, err := s.sources(); err != nil {
		return nil, err
	}

	point1, err := pointFromCommand(cmd, "location_1")
	if err != nil {
		return nil, fmt.Errorf("%s: %w", usage, err)
	}
	point2, err := pointFromCommand(cmd, "location_2")
	if err != nil {
		return nil, fmt.Errorf("%s: %w", usage, err)
	}

	return calculateDistances(point1, point2), nil
}

// Close shuts the component down. The two location sensors are dependencies the
// machine owns and closes itself, so Close leaves them alone and only stops this
// component from reading them: any later call to Readings or DoCommand reports
// the component as closed. Swap makes it idempotent.
func (s *haversineSensor) Close(ctx context.Context) error {
	if s.closed.Swap(true) {
		return nil
	}

	s.logger.Debug("Haversine sensor closed")
	return nil
}

// coordinate is a latitude/longitude pair in degrees.
type coordinate struct {
	lat float64
	lng float64
}

func (c coordinate) validate() error {
	if c.lat < -90 || c.lat > 90 {
		return fmt.Errorf("latitude %v is out of range [-90, 90]", c.lat)
	}
	if c.lng < -180 || c.lng > 180 {
		return fmt.Errorf("longitude %v is out of range [-180, 180]", c.lng)
	}
	return nil
}

func pointFromCommand(cmd map[string]interface{}, key string) (coordinate, error) {
	raw, ok := cmd[key]
	if !ok {
		return coordinate{}, fmt.Errorf("missing %q", key)
	}
	location, ok := raw.(map[string]interface{})
	if !ok {
		return coordinate{}, fmt.Errorf("%q must be an object, got %T", key, raw)
	}

	lat, err := toFloat(location["latitude"])
	if err != nil {
		return coordinate{}, fmt.Errorf("%q latitude: %w", key, err)
	}
	lng, err := toFloat(location["longitude"])
	if err != nil {
		return coordinate{}, fmt.Errorf("%q longitude: %w", key, err)
	}

	point := coordinate{lat: lat, lng: lng}
	if err := point.validate(); err != nil {
		return coordinate{}, fmt.Errorf("%q: %w", key, err)
	}
	return point, nil
}

// point extracts the coordinate at the source's configured latitude and longitude paths.
func (src *source) point(readings map[string]interface{}) (coordinate, error) {
	lat, err := floatAtPath(readings, src.latPath)
	if err != nil {
		return coordinate{}, err
	}
	lng, err := floatAtPath(readings, src.lngPath)
	if err != nil {
		return coordinate{}, err
	}

	point := coordinate{lat: lat, lng: lng}
	if err := point.validate(); err != nil {
		return coordinate{}, err
	}
	return point, nil
}

// readingValid reports whether a reading is recent enough. A source without
// both an updated path and an expire duration is always valid. A timestamp that
// cannot be read is treated as expired.
func (s *haversineSensor) readingValid(readings map[string]interface{}, src *source) bool {
	if src.updatedPath == nil || src.expire == 0 {
		return true
	}

	raw, err := valueAtPath(readings, src.updatedPath)
	if err != nil {
		s.logger.Warnf("Error checking reading validity: %v", err)
		return false
	}
	updated, err := toTime(raw)
	if err != nil {
		s.logger.Warnf("Error checking reading validity: %v", err)
		return false
	}

	return time.Since(updated) <= src.expire
}

func calculateDistances(point1, point2 coordinate) map[string]interface{} {
	km := haversineKm(point1, point2)
	return map[string]interface{}{
		"distance_km":             km,
		"distance_miles":          km * kmToMiles,
		"distance_nautical_miles": km * kmToNauticalMiles,
		"location_1": map[string]interface{}{
			"latitude":  point1.lat,
			"longitude": point1.lng,
		},
		"location_2": map[string]interface{}{
			"latitude":  point2.lat,
			"longitude": point2.lng,
		},
	}
}

// haversineKm returns the great-circle distance in kilometers between two
// points on a sphere of the Earth's average radius.
func haversineKm(point1, point2 coordinate) float64 {
	lat1, lng1 := radians(point1.lat), radians(point1.lng)
	lat2, lng2 := radians(point2.lat), radians(point2.lng)

	sinLat := math.Sin((lat2 - lat1) / 2)
	sinLng := math.Sin((lng2 - lng1) / 2)
	h := sinLat*sinLat + math.Cos(lat1)*math.Cos(lat2)*sinLng*sinLng

	return 2 * earthRadiusKm * math.Asin(math.Sqrt(h))
}

func radians(degrees float64) float64 {
	return degrees * math.Pi / 180
}

// durationPattern matches the config's duration strings: 1d, 12h, 10m, 30s, 100ms.
var durationPattern = regexp.MustCompile(`^(\d+)(d|h|m|s|ms)$`)

func parseDuration(s string) (time.Duration, error) {
	match := durationPattern.FindStringSubmatch(s)
	if match == nil {
		return 0, fmt.Errorf("invalid duration format: %s. Expected format like '1d', '12h', '10m', '30s', '100ms'", s)
	}

	value, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid duration value: %s", s)
	}

	switch match[2] {
	case "d":
		return time.Duration(value) * 24 * time.Hour, nil
	case "h":
		return time.Duration(value) * time.Hour, nil
	case "m":
		return time.Duration(value) * time.Minute, nil
	case "s":
		return time.Duration(value) * time.Second, nil
	default: // "ms"
		return time.Duration(value) * time.Millisecond, nil
	}
}

// floatAtPath reads the value at path and converts it to a float.
func floatAtPath(readings map[string]interface{}, path []string) (float64, error) {
	value, err := valueAtPath(readings, path)
	if err != nil {
		return 0, err
	}
	number, err := toFloat(value)
	if err != nil {
		return 0, fmt.Errorf("could not convert value to float at path %s: %w", strings.Join(path, "."), err)
	}
	return number, nil
}

// valueAtPath walks a dot-separated path through a sensor reading. Each step
// indexes a map or reads a field off a structured value such as a GeoPoint. A
// final value wrapped in a "value" key is unwrapped, which is how some sensors report scalars.
func valueAtPath(readings map[string]interface{}, path []string) (interface{}, error) {
	var current interface{} = readings

	for _, key := range path {
		next, err := childValue(current, key)
		if err != nil {
			return nil, fmt.Errorf("path %s: %w", strings.Join(path, "."), err)
		}
		current = next
	}

	if wrapper, ok := current.(map[string]interface{}); ok {
		if value, ok := wrapper["value"]; ok {
			return value, nil
		}
	}
	return current, nil
}

func childValue(current interface{}, key string) (interface{}, error) {
	switch parent := current.(type) {
	case map[string]interface{}:
		value, ok := parent[key]
		if !ok {
			return nil, fmt.Errorf("key %q not found", key)
		}
		return value, nil
	case *geo.Point:
		switch strings.ToLower(key) {
		case "lat", "latitude":
			return parent.Lat(), nil
		case "lng", "lon", "long", "longitude":
			return parent.Lng(), nil
		}
		return nil, fmt.Errorf("key %q not found on geo point", key)
	}

	return fieldValue(current, key)
}

// fieldValue reads key off an arbitrary map or struct, so paths can reach into
// values a sensor reports as Go types rather than plain maps.
func fieldValue(current interface{}, key string) (interface{}, error) {
	value := reflect.ValueOf(current)
	for value.Kind() == reflect.Pointer || value.Kind() == reflect.Interface {
		if value.IsNil() {
			return nil, fmt.Errorf("cannot access %q, value is nil", key)
		}
		value = value.Elem()
	}

	switch value.Kind() {
	case reflect.Map:
		if value.Type().Key().Kind() != reflect.String {
			break
		}
		entry := value.MapIndex(reflect.ValueOf(key).Convert(value.Type().Key()))
		if !entry.IsValid() {
			return nil, fmt.Errorf("key %q not found", key)
		}
		return entry.Interface(), nil
	case reflect.Struct:
		field := value.FieldByNameFunc(func(name string) bool {
			return strings.EqualFold(name, key)
		})
		if field.IsValid() && field.CanInterface() {
			return field.Interface(), nil
		}
		return nil, fmt.Errorf("field %q not found on %T", key, current)
	default:
	}

	return nil, fmt.Errorf("cannot access %q, parent is not a map, struct, or geo point: %v", key, current)
}

func toFloat(value interface{}) (float64, error) {
	switch typed := value.(type) {
	case nil:
		return 0, fmt.Errorf("value is missing")
	case float64:
		return typed, nil
	case float32:
		return float64(typed), nil
	case int:
		return float64(typed), nil
	case int32:
		return float64(typed), nil
	case int64:
		return float64(typed), nil
	case uint64:
		return float64(typed), nil
	case string:
		number, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if err != nil {
			return 0, fmt.Errorf("%q is not a number", typed)
		}
		return number, nil
	default:
		return 0, fmt.Errorf("unsupported value type %T", value)
	}
}

// timeLayouts covers the ISO 8601 timestamps sensors report. The zoneless
// layouts are parsed in the local zone, matching how the reading's age is
// measured against the local clock.
var timeLayouts = []struct {
	layout string
	local  bool
}{
	{time.RFC3339Nano, false},
	{time.RFC3339, false},
	{"2006-01-02T15:04:05.999999999", true},
	{"2006-01-02T15:04:05", true},
	{"2006-01-02 15:04:05.999999999", true},
	{"2006-01-02 15:04:05", true},
	{"2006-01-02", true},
}

func toTime(value interface{}) (time.Time, error) {
	switch typed := value.(type) {
	case time.Time:
		return typed, nil
	case string:
		// Python's fromisoformat accepts a trailing Z only as an offset.
		text := strings.TrimSpace(typed)
		for _, candidate := range timeLayouts {
			if candidate.local {
				if parsed, err := time.ParseInLocation(candidate.layout, text, time.Local); err == nil {
					return parsed, nil
				}
				continue
			}
			if parsed, err := time.Parse(candidate.layout, text); err == nil {
				return parsed, nil
			}
		}
		return time.Time{}, fmt.Errorf("could not parse timestamp %q", text)
	default:
		return time.Time{}, fmt.Errorf("unsupported timestamp type %T", value)
	}
}

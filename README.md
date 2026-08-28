# Viam Haversine Component

A Viam sensor component that calculates the distance between two geographical points using the haversine formula.

This component can either:
1. Calculate distances between data from two configured sensors (via `GetReadings`)
2. Calculate distances between two manually provided coordinates (via `DoCommand`)

The distances are provided in three units:
- Kilometers
- Miles
- Nautical Miles

## Model viam-soleng:haversine:haversine

This model implements a sensor component that calculates the great-circle distance between two points on a sphere using the haversine formula.

### Supported Sensor Types

This component supports both **Sensor** and **MovementSensor** components as data sources. It resolves each configured sensor by name under either API, and reads it through the readings API both types share.

### Configuration

The following configuration template shows how to set up the haversine component:

```json
{
  "sensor_1": {
    "name": "<sensor_name>",
    "latitude": "<path.to.latitude>",
    "longitude": "<path.to.longitude>",
    "updated": "<path.to.timestamp>",
    "expire": "<duration>"
  },
  "sensor_2": {
    "name": "<sensor_name>",
    "latitude": "<path.to.latitude>",
    "longitude": "<path.to.longitude>",
    "updated": "<path.to.timestamp>",
    "expire": "<duration>"
  }
}
```

#### Attributes

The following attributes are optional for this model:

| Name | Type | Inclusion | Description |
|------|------|-----------|-------------|
| `sensor_1` | object | Optional | Configuration for the first location sensor (Sensor or MovementSensor) |
| `sensor_2` | object | Optional | Configuration for the second location sensor (Sensor or MovementSensor) |

If you don't configure both sensors, only `DoCommand` will be fully functional. `GetReadings` returns an empty object if both sensors are not configured.

Each sensor configuration requires:
- `name`: The name of the sensor component (can be either Sensor or MovementSensor)
- `latitude`: Dot-separated path to the latitude value in the sensor's readings
- `longitude`: Dot-separated path to the longitude value in the sensor's readings

Each sensor configuration optionally supports:
- `updated`: Path to an ISO 8601 timestamp field. Both the extended and basic forms are accepted, with the date and time separated by either `T` or a space, and an offset written as `Z`, `+00:00`, `+0000`, or `+00` — e.g. `2023-12-01T10:30:00Z`, `2023-12-01 10:30:00+00:00`, `20231201T103000Z`. A timestamp with no offset is read in the machine's local time zone.
- `expire`: Duration string after which the reading is considered stale (e.g., "1d", "12h", "10m", "30s", "100ms")

`updated` and `expire` must be configured together: staleness cannot be checked with only one of them, so configuring one alone is a validation error. When both are set, the reading is considered invalid and no distance is calculated once the timestamp is older than the expire duration.

A path step indexes a map key, or reads a field off a structured reading. A movement sensor reports its position as a geo point, so `position.lat` and `position.lng` both work, as do the aliases `position.latitude` and `position.longitude`.

#### Example Configuration

```json
{
  "sensor_1": {
    "name": "gps1",
    "latitude": "position.lat",
    "longitude": "position.lng",
    "updated": "timestamp",
    "expire": "5m"
  },
  "sensor_2": {
    "name": "phone_data",
    "latitude": "loc.latitude",
    "longitude": "loc.longitude",
    "updated": "last_updated",
    "expire": "1h"
  }
}
```

In this example `gps1` is a MovementSensor (a GPS module) and `phone_data` is a regular Sensor (a data source from a phone app).

### Methods

#### GetReadings

Returns the distance between the two configured sensors. The method will:
1. Return an empty object (`{}`) if both sensors are not configured
2. Read both configured sensors
3. Return an empty object (`{}`) if either reading has expired, when `updated` and `expire` are configured
4. Extract latitude and longitude using the configured paths
5. Calculate distances in multiple units

Example response when sensors are configured and readings are valid:
```json
{
  "distance_km": 392.21,
  "distance_miles": 243.71,
  "distance_nautical_miles": 211.78,
  "location_1": {
    "latitude": 45.7597,
    "longitude": 4.8422
  },
  "location_2": {
    "latitude": 48.8567,
    "longitude": 2.3508
  }
}
```

#### DoCommand

Calculates the distance between two manually provided coordinates. This method works regardless of sensor configuration, making it useful for one-off distance calculations or when you don't have physical sensors.

Example command:
```json
{
  "location_1": {
    "latitude": 45.7597,
    "longitude": 4.8422
  },
  "location_2": {
    "latitude": 48.8567,
    "longitude": 2.3508
  }
}
```

The response format is identical to `GetReadings`.

### Error Handling

The component will:
- Return an empty object from `GetReadings` if both sensors are not configured
- Return an empty object from `GetReadings` if either sensor reading has expired
- Log a warning during startup if a sensor is configured but not found among the dependencies
- Log a warning if a reading has expired or its timestamp cannot be read
- Return an error if:
  - A sensor cannot be read
  - Sensor readings don't contain a value at the configured path, or that value is not a number
  - `DoCommand` is missing `location_1` or `location_2`, or either is not a latitude/longitude object
  - A coordinate is outside the range [-90, 90] latitude or [-180, 180] longitude
  - The component has been closed, which happens when the machine reconfigures or shuts it down

Configuration errors are reported at validation time: a configured sensor missing `name`, `latitude`, or `longitude`, an `updated` or `expire` set without the other, or an `expire` value that isn't a valid duration.

## Building

The module is written in Go and builds to a single static binary.

```bash
make build      # host binary at bin/haversine
make test       # run the unit tests
make packages   # cross-compile a tarball per platform under bin/<goos>-<goarch>/
make upload     # cross-compile, then print the viam module upload commands
```

Cloud builds run `make module.tar.gz`, which packages `bin/haversine` and `meta.json` at the root of the tarball. Windows builds are cross-compiled with a `.exe` suffix, and their `meta.json` entrypoint is rewritten to match.

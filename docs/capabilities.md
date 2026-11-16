<!-- Generated from internal/capability by its tests: go test ./internal/capability -update -->

# Capabilities

Every capability Oikade knows, named for people rather than for a protocol ([OIK-181](https://linear.app/oikade/issue/OIK-181)). A home names them through `oikade`'s constants and the handles `oikade types` writes, and a plugin through `contract/v1`'s. Any other name is unknown.

| Name | In a home | Type | Range | Writable | Matter |
|---|---|---|---|---|---|
| `battery` | `oikade.Battery` | number | 0 to 100% |  | Power Source (0x002F) BatPercentRemaining (0x000C) |
| `brightness` | `oikade.Brightness` | number | 0 to 100% | yes | Level Control (0x0008) CurrentLevel (0x0000) |
| `humidity` | `oikade.Humidity` | number | 0 to 100% |  | Relative Humidity Measurement (0x0405) MeasuredValue (0x0000) |
| `link-quality` | `oikade.LinkQuality` | number | 0 to 255 |  | none |
| `locked` | `oikade.Locked` | bool |  | yes | Door Lock (0x0101) LockState (0x0000) |
| `occupied` | `oikade.Occupied` | bool |  |  | Occupancy Sensing (0x0406) Occupancy (0x0000) |
| `on` | `oikade.On` | bool |  | yes | On/Off (0x0006) OnOff (0x0000) |
| `open` | `oikade.Open` | bool |  |  | Boolean State (0x0045) StateValue (0x0000), inverted |
| `position` | `oikade.Position` | number | 0 to 100% | yes | Window Covering (0x0102) CurrentPositionLiftPercent100ths (0x000E) |
| `setpoint` | `oikade.Setpoint` | number | 0 to 50°C | yes | Thermostat (0x0201) OccupiedHeatingSetpoint (0x0012) |
| `temperature` | `oikade.Temperature` | number | -273.15 to 1000°C |  | Temperature Measurement (0x0402) MeasuredValue (0x0000) |

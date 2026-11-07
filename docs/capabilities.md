<!-- Generated from internal/capability by its tests: go test ./internal/capability -update -->

# Capabilities

Every capability Oikade knows, named for people rather than for a protocol ([OIK-181](https://linear.app/oikade/issue/OIK-181)). A home names them through `framework`'s constants and the handles `oikade types` writes, and a plugin through `contract/v1`'s. Any other name is unknown.

| Name | In a home | Type | Range | Writable | Matter |
|---|---|---|---|---|---|
| `battery` | `framework.Battery` | number | 0 to 100% |  | Power Source (0x002F) BatPercentRemaining (0x000C) |
| `brightness` | `framework.Brightness` | number | 0 to 100% | yes | Level Control (0x0008) CurrentLevel (0x0000) |
| `humidity` | `framework.Humidity` | number | 0 to 100% |  | Relative Humidity Measurement (0x0405) MeasuredValue (0x0000) |
| `link-quality` | `framework.LinkQuality` | number | 0 to 255 |  | none |
| `occupied` | `framework.Occupied` | bool |  |  | Occupancy Sensing (0x0406) Occupancy (0x0000) |
| `on` | `framework.On` | bool |  | yes | On/Off (0x0006) OnOff (0x0000) |
| `open` | `framework.Open` | bool |  |  | Boolean State (0x0045) StateValue (0x0000), inverted |
| `temperature` | `framework.Temperature` | number | -273.15 to 1000°C |  | Temperature Measurement (0x0402) MeasuredValue (0x0000) |

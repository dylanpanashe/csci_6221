# CampusPulse architecture

CampusPulse estimates how busy campus spaces are and how well the Wi-Fi works in them. It combines a synthetic timetable with simulated Wi-Fi observations. Everything runs in one Go binary, which also serves the browser frontend.

This document covers the overall design. `DATA_PIPELINE.md`, `PRIVACY.md` and `GO_CONCEPTS.md` go deeper on their topics.

> **Status:** Stage 1 is in place: domain model, campus layout, interfaces, configuration, the simulation clock and the application lifecycle. Sections marked *(planned)* describe stages still to be built.

## Running shape

```
                    ┌──────────────────────── Go process ────────────────────────┐
                    │                                                             │
 SimClock ─────────►│  Simulator (planned)        Pipeline (planned)              │
 (accelerated time) │  ├─ timetable generator ──► timetable events ──┐            │
                    │  ├─ student movement                           │            │
                    │  └─ Wi-Fi + AP telemetry ─► observations ──────┤            │
                    │                              │                 ▼            │
                    │                    validate → privacy → aggregate → fuse    │
                    │                                                  │          │
                    │                       forecast ◄─────────────────┤          │
                    │                       diagnostics → alerts ◄─────┘          │
                    │                                  │                          │
                    │                          state store (Repository)           │
                    │                         ▲                    │              │
                    │      REST API ──────────┘          WebSocket hub            │
                    └──────────┬───────────────────────────────┬──────────────────┘
                               │ JSON                          │ push every 2 s
                               ▼                               ▼
                         Browser: Three.js campus, dashboards, controls
```

## Packages

| Package | Responsibility | Status |
|---|---|---|
| `main` | Parse config, set up logging, trap Ctrl-C/SIGTERM, run the app | done |
| `internal/campus` | Domain types, the GWU campus layout, sentinel and custom errors, the interfaces everything is wired through | done |
| `internal/config` | Defaults, `CAMPUSPULSE_*` environment variables, flags, validation | done |
| `internal/simclock` | Accelerated, pausable simulation clock; fixed clock for tests | done |
| `internal/app` | Builds and connects components; owns start-up and graceful shutdown | done (grows each stage) |
| `internal/workerpool` | Generic bounded worker pool and fan-out/fan-in helpers | planned |
| `internal/fn` | Generic `Map`, `Filter`, `Reduce`, function composition | planned |
| `internal/simulator` | Timetable, student movement, Wi-Fi observations, AP telemetry, faults, ground truth | planned |
| `internal/privacy` | Daily rotating device hashing, small-group suppression, noise | planned |
| `internal/occupancy` | Device aggregation, timetable/Wi-Fi fusion, room availability | planned |
| `internal/forecast` | Rule-based 15/30/60/120-minute forecasts and changeover warnings | planned |
| `internal/diagnostics` | Rule registry (functions as values) separating congestion from dead zones and failures | planned |
| `internal/alerts` | Alert lifecycle, de-duplication and detection timing | planned |
| `internal/pipeline` | Channel stages connecting the components above | planned |
| `internal/store` | Thread-safe state store implementing `campus.Repository` | planned |
| `internal/api` | REST handlers and middleware | planned |
| `internal/websocket` | Hub, per-client write pumps, slow-client policy | planned |
| `web/` | HTML, CSS and vanilla JS with Three.js from a CDN | placeholder |

### Why the domain lives in one package

The suggested layout had separate `campus`, `buildings` and `rooms` packages. They were merged into `internal/campus` because buildings, floors and rooms point at each other (a room knows its floor and building, a building lists its floors). Separate packages would need import cycles or a pile of extra interfaces just to break them. One cohesive domain package is the normal Go answer.

## The simulated campus

`campus.NewGWUCampus()` builds the campus from a declarative table in `layout.go`.

| Building | Style hint | Floor height | Notable spaces |
|---|---|---|---|
| Bell Hall | brick, classical | 4.4 m | Lecture Hall 101 (300 seats), East Wing Study Room 203 (planted dead zone) |
| Science & Engineering Hall | glass, modern | 5.0 m | Lecture Hall B110 (180 seats), five labs, planted dead zone on floor 3 west |
| Gelman Library | brutalist | 4.0 m | Reading rooms and group study, planted dead zone on floor 1 east |

Each building has 3 floors and 4 rooms per floor, which makes 36 rooms. Each floor also has a lobby space covering stairs, lifts and corridors, so changeover crowds have somewhere to show up.

Every floor has a west-wing AP, an east-wing AP and a lobby AP. Lecture halls also get a dedicated high-density AP. That gives 29 APs in total. Every room lists neighbouring APs too, so the simulator can model signal leaking between rooms.

Positions are in metres. X runs west to east, Z runs north to south, and Y is height. The Three.js scene uses the same axes, so the backend's coordinates go straight into the renderer.

## Data ownership and concurrency rules

1. **The campus topology is immutable.** `Campus`, `Building`, `Floor`, `Room` and `AccessPoint` are built once and never modified. Any goroutine can read them without locks.
2. **Changing state has one owner.** Occupancy estimates, AP telemetry and alerts live in the state store. It guards them with an `RWMutex` and hands out copies, never pointers into its maps. *(planned)*
3. **Channels have one closer.** Whoever creates a channel closes it, after every sender has finished. Producers receive send-only channels (`chan<- T`), so the compiler stops them closing or reading what they don't own.
4. **Ground truth stays in the simulator.** True headcounts are used only to measure accuracy. They never reach the public API or the WebSocket stream. *(planned)*
5. **Time comes from `campus.Clock`.** Nothing in the pipeline calls `time.Now()` directly. That's how the simulation can run at 60x speed, pause, and be tested with a fixed clock.

## Interfaces

These are defined in `internal/campus/ports.go`. They are small and describe behaviour.

| Interface | Implemented by | Used by |
|---|---|---|
| `Clock` | `simclock.SimClock`, `simclock.Fixed` | everything time-dependent |
| `ObservationSource`, `TelemetrySource` | simulator; test fakes | pipeline ingest |
| `Timetable` | simulator timetable | fusion, forecasting, room finder |
| `OccupancyEstimator` | fusion estimator | pipeline |
| `ForecastEngine` | rule-based forecaster | API, broadcaster |
| `DiagnosticEngine` | rule registry | pipeline |
| `EventPublisher` | WebSocket hub; test recorder | pipeline broadcaster |
| `ReadRepository` / `Repository` | state store | API (read only) / pipeline (read-write) |

`Repository` is composed from smaller reader and writer interfaces. HTTP handlers receive only `ReadRepository`, so a handler cannot change state even by mistake.

## Lifecycle

1. `main` loads config, creates a `slog` logger and a context that is cancelled on Ctrl-C or SIGTERM.
2. `app.New` builds the campus, clock and HTTP server. Nothing runs yet.
3. `app.Run` starts the HTTP server in a goroutine and, in later stages, the simulator, pipeline and hub. Every goroutine is tracked by a `sync.WaitGroup`.
4. When the context is cancelled, `Run` stops the HTTP server with a timeout. Cancellation flows down to every worker, channels drain and close in pipeline order, and `Run` waits for the `WaitGroup` before returning.

## Configuration

| Flag | Env var | Default | Meaning |
|---|---|---|---|
| `-addr` | `CAMPUSPULSE_ADDR` | `:8080` | HTTP listen address |
| `-tick` | `CAMPUSPULSE_TICK` | `2s` | real time between ticks and broadcasts |
| `-speed` | `CAMPUSPULSE_SPEED` | `60` | simulated seconds per real second |
| `-start-hour` | `CAMPUSPULSE_START_HOUR` | `8` | simulated day starts at this hour |
| `-population` | `CAMPUSPULSE_POPULATION` | `2600` | simulated students at peak |
| `-demo` | `CAMPUSPULSE_DEMO` | `false` | start at the Bell Hall 1:40 PM scenario |
| `-devices-per-user` | `CAMPUSPULSE_DEVICES_PER_USER` | `1.8` | calibration factor |
| `-suppress-below` | `CAMPUSPULSE_SUPPRESS_BELOW` | `10` | published counts under this show as `<10` |
| `-noise` | `CAMPUSPULSE_NOISE` | `1.0` | std-dev of privacy noise |
| `-workers` | `CAMPUSPULSE_WORKERS` | CPU count | observation worker pool size |
| `-pprof` | `CAMPUSPULSE_PPROF` | `false` | expose `/debug/pprof` (development only) |
| `-log-level` | | `info` | `debug`, `info`, `warn`, `error` |
| | `CAMPUSPULSE_TZ` | `America/New_York` | campus time zone |
| | `CAMPUSPULSE_HASH_SECRET` | random per run | base secret for device hashing |

Time zone data is embedded with `time/tzdata`, so the zone works in minimal Docker images too.

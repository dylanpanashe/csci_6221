# CampusPulse architecture

CampusPulse estimates how busy campus spaces are and how well the Wi-Fi works in them. It combines a synthetic timetable with simulated Wi-Fi observations. Everything runs in one Go binary, which also serves the browser frontend.

This document covers the overall design. `DATA_PIPELINE.md`, `PRIVACY.md` and `GO_CONCEPTS.md` go deeper on their topics.

> **Status:** The backend is complete (Stages 1 to 4): domain model, simulator, data pipeline, fusion, forecasting, diagnostics, alerts, state store, accuracy evaluation, REST API, middleware and WebSocket hub. Stages 5 to 7 add the 3D frontend.

## Running shape

```
                    ┌──────────────────────── Go process ────────────────────────┐
                    │                                                             │
 SimClock ─────────►│  Simulator                  Pipeline                        │
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
| `internal/workerpool` | Generic bounded worker pool with panic recovery, atomic stats and a `Merge` fan-in helper | done |
| `internal/fn` | Generic `Map`, `Filter`, `Reduce`, `GroupBy`, predicate combinators, `Chain` and `Compose` | done |
| `internal/simulator` | Timetable, student movement (sharded), per-building Wi-Fi zones, AP telemetry, faults, controls, ground truth | done |
| `internal/privacy` | Daily rotating HMAC device hashing, small-group suppression, noise | done |
| `internal/occupancy` | Timetable/Wi-Fi fusion, room availability, privacy-safe publication, group totals | done |
| `internal/forecast` | Rule-based 15/30/60/120-minute forecasts (strategy registry), learned patterns, changeover warnings | done |
| `internal/diagnostics` | Rule registry (functions as values) separating congestion, dead zones, failures and degradation | done |
| `internal/alerts` | Alert lifecycle, de-duplication, hysteresis, detection timing | done |
| `internal/pipeline` | Channel stages: source → worker pool (validate, normalise, hash, pre-aggregate) → aggregator | done |
| `internal/store` | RWMutex state store implementing `campus.Repository`, generic `Ring[T]` history | done |
| `internal/events` | Generic non-blocking publish/subscribe `Bus[T]` | done |
| `internal/engine` | Runs fusion, forecasting, diagnostics, alerts, history and evaluation as connected goroutines | done |
| `internal/accuracy` | Scores estimates, forecasts and diagnoses against simulator ground truth | done |
| `internal/api` | REST handlers, middleware chain, control registry, building brief | done |
| `internal/finder` | Study-space filters (composable predicates) and ranking | done |
| `internal/websocket` | Hub, per-client write pumps, topic subscriptions, slow-client policy | done |
| `web/` | HTML, CSS and vanilla JS with Three.js from a CDN | Stage 5 |

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

## How the simulator runs concurrently

Each tick (every 2 s of real time by default) the simulator loop:

1. Sends the tick to 4 **student shard** goroutines. Each shard owns 650 students and moves them toward where their timetable says they should be. Students walk through floor lobbies, down to the ground floor, outdoors between buildings and back up. Lobby dwell time grows with how crowded the lobby was on the previous tick, so lift and stair queues build up during changeovers.
2. Collects the shard results (fan-in) and merges them into ground truth: the true headcount in every room.
3. Sends each **building zone** goroutine the devices present in its building. A zone owns its access points. It decides which AP each device associates with (neighbouring APs pick up some devices, more so in dead zones), what signal it sees, which room the controller's location service thinks it is in (wrong about 15% of the time), and the AP's utilisation, latency and packet loss. Failed APs send nothing, just like real hardware.
4. Emits the observations in 512-reading batches, the AP telemetry, and a public activity frame.

Shards and zones share no mutable state, so none of this needs a lock. Control commands (pause, fail an AP, trigger a changeover) are sent to the loop over a channel and applied between ticks. The current schedule and the latest ground truth are swapped in with `atomic.Pointer`, so other goroutines can read them without blocking the loop.

## Data ownership and concurrency rules

1. **The campus topology is immutable.** `Campus`, `Building`, `Floor`, `Room` and `AccessPoint` are built once and never modified. Any goroutine can read them without locks.
2. **Changing state has one owner.** Occupancy estimates, AP telemetry and alerts live in the state store. It guards them with an `RWMutex` and hands out copies, never pointers into its maps. Stateful helpers (the fusion smoother, the diagnostics window, the alert manager) are each owned by exactly one engine goroutine and need no lock at all.
3. **Channels have one closer.** Whoever creates a channel closes it, after every sender has finished. Producers receive send-only channels (`chan<- T`), so the compiler stops them closing or reading what they don't own.
4. **Ground truth stays in the simulator.** True headcounts are used only by `internal/accuracy` to measure error. They never reach the public API or the WebSocket stream. See `PRIVACY.md`.
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

## REST API

Every response uses the same envelope:

```json
{"ok": true,  "data": { ... }, "meta": {"request_id": "9f2c1a0b-000042", "sim_time": "2026-11-18T13:50:00-05:00"}}
{"ok": false, "error": {"code": "not_found", "message": "room \"bell-999\" not found"}, "meta": { ... }}
```

Errors are mapped to status codes in one place (`api.classify`) using `errors.Is` and `errors.As`: not found → 404, validation → 400, simulator not running → 503, timeout → 504, anything else → 500.

| Method and path | Returns |
|---|---|
| `GET /api/health` | status, uptime, simulator state |
| `GET /api/campus` | layout (buildings, floors, rooms, APs, walkways) plus campus summary: occupancy, forecast, busy buildings, free study rooms, Wi-Fi health, alerts, trend |
| `GET /api/buildings` | per-building occupancy, forecast, Wi-Fi summary, alert and warning counts |
| `GET /api/buildings/{id}` | summary, floors, upcoming classes, changeover warnings, best study rooms, and the student **brief** (the slide-15 view) |
| `GET /api/buildings/{id}/floors` | floors with room-level occupancy, Wi-Fi, current and next class |
| `GET /api/rooms/{id}` | room details, estimate, free seats, Wi-Fi quality and diagnosis, today's timetable, forecast |
| `GET /api/rooms?building=&level=&type=&min_free=&wifi=&for=&max_level=&limit=` | study-space finder |
| `GET /api/recommendations/quiet?minutes=60&wifi=optional` | "find me a quiet place to study for the next hour" |
| `GET /api/wifi/access-points?building=&health=` | every AP with live status |
| `GET /api/wifi/access-points/{id}` | one AP with latency and utilisation history |
| `GET /api/wifi/dead-zones` | APs currently diagnosed as dead zones, with affected rooms |
| `GET /api/alerts?active=false&building=&limit=` | alerts, newest first |
| `GET /api/forecast?kind=room\|floor\|building\|campus&subject=` | 15/30/60/120-minute forecast with reasons, plus changeover warnings |
| `GET /api/changeovers` | changeover warnings for the next 3 hours |
| `GET /api/history?series=a,b` | time series for charts |
| `GET /api/metrics` | pipeline, hub, runtime and accuracy figures |
| `GET /api/accuracy` | scorecard against the proposal's targets |
| `GET /api/simulation` | speed, paused, activity level |
| `POST /api/simulation/control` | `{"action": ...}`: `pause`, `resume`, `reset`, `speed` (`value`), `activity_up`, `activity_down`, `changeover`, `fail_ap` / `restore_ap` (`ap_id`), `demo` |
| `GET /ws` | WebSocket live updates |
| `GET /debug/pprof/...` | Go profiler, only with `-pprof` |

### Middleware

`api.Chain` composes plain `func(http.Handler) http.Handler` values, outermost first:

`RequestID → Recover → Logging → Timing → SecurityHeaders → CORS → ValidateJSON → router`

`/ws` skips the response-wrapping middleware. The WebSocket upgrade must hijack the raw connection, and the wrappers would hide it.

## WebSocket protocol

Every server message has the same shape: `{"type": "...", "at": "<sim time>", "data": ...}`.

| Type | When | Data |
|---|---|---|
| `occupancy` | every tick | published room estimates and floor/building/campus totals |
| `wifi` | every tick | status of every AP |
| `alert` | when raised | one alert |
| `alerts` | on connect | open alerts |
| `forecast` | every tick | building, floor and campus forecasts plus changeover warnings |
| `frame` | every tick | sim time, speed, pause state, activity, coarse movement flows, class starts and ends |

A new client first receives a snapshot of current state, then live updates. Clients can send:

```json
{"type": "subscribe", "topics": ["occupancy", "wifi"]}
{"type": "prefs", "flows": false}
```

The UI's visualisation toggles use these, so switching off the Wi-Fi layer or the student animation also stops the server sending that data.

**Slow clients.** Each client has a 64-message queue and one writer goroutine. If the queue is full the message is dropped for that client only. After 20 drops in a row the client is disconnected; the browser reconnects and gets a fresh snapshot. The hub never waits on a client.

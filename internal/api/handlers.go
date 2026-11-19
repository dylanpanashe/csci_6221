package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/campus"
	"github.com/dylanpanashe/csci_6221/internal/diagnostics"
	"github.com/dylanpanashe/csci_6221/internal/finder"
	"github.com/dylanpanashe/csci_6221/internal/fn"
	"github.com/dylanpanashe/csci_6221/internal/forecast"
	"github.com/dylanpanashe/csci_6221/internal/store"
)

// ---------------------------------------------------------------------------
// Response views
// ---------------------------------------------------------------------------

// WiFiSummary counts APs by health.
type WiFiSummary struct {
	Total     int                `json:"total"`
	Healthy   int                `json:"healthy"`
	Congested int                `json:"congested"`
	Degraded  int                `json:"degraded"`
	Offline   int                `json:"offline"`
	Quality   campus.WiFiQuality `json:"quality"` // most common room quality
}

// BuildingSummary is one row of /api/buildings.
type BuildingSummary struct {
	ID           campus.BuildingID        `json:"id"`
	Name         string                   `json:"name"`
	Occupancy    campus.OccupancyEstimate `json:"occupancy"`
	Forecast     []campus.ForecastPoint   `json:"forecast"`
	WiFi         WiFiSummary              `json:"wifi"`
	ActiveAlerts int                      `json:"active_alerts"`
	Warnings     int                      `json:"changeover_warnings"`
}

// FloorView is a floor with its rooms.
type FloorView struct {
	ID         campus.FloorID           `json:"id"`
	Level      int                      `json:"level"`
	Occupancy  campus.OccupancyEstimate `json:"occupancy"`
	Forecast   []campus.ForecastPoint   `json:"forecast"`
	WiFi       WiFiSummary              `json:"wifi"`
	ClassNow   bool                     `json:"class_now"`             // a class is running on this floor
	QuietUntil *time.Time               `json:"quiet_until,omitempty"` // next class; nil with ClassNow false = none today
	Rooms      []RoomSummary            `json:"rooms"`
	APs        []campus.AccessPointID   `json:"access_points"`
}

// RoomSummary is a room inside a floor listing.
type RoomSummary struct {
	ID        campus.RoomID            `json:"id"`
	Name      string                   `json:"name"`
	Type      campus.RoomType          `json:"type"`
	Wing      campus.Wing              `json:"wing"`
	Estimate  campus.OccupancyEstimate `json:"estimate"`
	Available bool                     `json:"available"`
	WiFi      campus.WiFiQuality       `json:"wifi"`
	Now       *campus.TimetableEvent   `json:"now,omitempty"`
	Next      *campus.TimetableEvent   `json:"next,omitempty"`
}

// APView joins an AP's layout with its live status.
type APView struct {
	ID       campus.AccessPointID `json:"id"`
	Label    string               `json:"label"`
	Building campus.BuildingID    `json:"building_id"`
	Level    int                  `json:"level"`
	Wing     campus.Wing          `json:"wing"`
	Position campus.Point         `json:"position"`
	Channel  int                  `json:"channel"`
	Capacity campus.DeviceCount   `json:"max_clients"`
	Rooms    []campus.RoomID      `json:"rooms"`
	Status   diagnostics.APStatus `json:"status"`
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func (s *Server) now() time.Time {
	if s.deps.Clock != nil {
		return s.deps.Clock.Now()
	}
	return time.Now()
}

func (s *Server) roomQuality(r *campus.Room) (campus.WiFiQuality, string) {
	st, ok := s.deps.State.Status(r.PrimaryAP)
	if !ok {
		return campus.WiFiGood, ""
	}
	return st.Quality, st.Primary.Detail
}

func (s *Server) wifiSummary(aps []*campus.AccessPoint, rooms []*campus.Room) WiFiSummary {
	w := WiFiSummary{Total: len(aps)}
	for _, ap := range aps {
		st, _ := s.deps.State.Status(ap.ID)
		switch st.Health {
		case campus.APCongested:
			w.Congested++
		case campus.APDegraded:
			w.Degraded++
		case campus.APOffline:
			w.Offline++
		default:
			w.Healthy++
		}
	}
	counts := map[campus.WiFiQuality]int{}
	for _, r := range rooms {
		q, _ := s.roomQuality(r)
		counts[q]++
	}
	best := 0
	for q := campus.WiFiExcellent; q <= campus.WiFiNone; q++ {
		if counts[q] > best {
			best, w.Quality = counts[q], q
		}
	}
	return w
}

func (s *Server) forecastPoints(kind, subject string) []campus.ForecastPoint {
	if f, ok := s.deps.State.Forecast(kind, subject); ok {
		return f.Points
	}
	return nil
}

func (s *Server) buildingAPs(b *campus.Building) (aps []*campus.AccessPoint, rooms []*campus.Room) {
	for _, f := range b.Floors {
		aps = append(aps, f.APs...)
		rooms = append(rooms, f.Rooms...)
	}
	return aps, rooms
}

func (s *Server) summarize(b *campus.Building) BuildingSummary {
	occ, _ := s.deps.State.Group(string(b.ID))
	aps, rooms := s.buildingAPs(b)
	sum := BuildingSummary{ID: b.ID, Name: b.Name, Occupancy: occ, Forecast: s.forecastPoints("building", string(b.ID)),
		WiFi: s.wifiSummary(aps, rooms)}
	for _, a := range s.deps.State.Alerts(true) {
		if a.BuildingID == b.ID {
			sum.ActiveAlerts++
		}
	}
	for _, w := range s.deps.State.Warnings() {
		if w.BuildingID == b.ID {
			sum.Warnings++
		}
	}
	return sum
}

func (s *Server) roomSummary(r *campus.Room, tt Timetable, now time.Time) RoomSummary {
	est, _ := s.deps.State.RoomEstimate(r.ID)
	q, _ := s.roomQuality(r)
	rs := RoomSummary{ID: r.ID, Name: r.Name, Type: r.Type, Wing: r.Wing, Estimate: est, WiFi: q}
	for _, e := range tt.RoomEvents(r.ID) {
		e := e
		if e.Cancelled {
			continue
		}
		if e.ActiveAt(now) {
			rs.Now = &e
		} else if e.Window.Start.After(now) && rs.Next == nil {
			rs.Next = &e
		}
	}
	rs.Available = rs.Now == nil && r.Type.IsStudyFriendly() && est.Level <= campus.OccupancyModerate
	return rs
}

// queryInt reads an optional integer query parameter.
func queryInt(r *http.Request, key string, def int) (int, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%w: %s must be a whole number", errBadRequest, key)
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	s.writeOK(w, r, map[string]any{
		"status":     "ok",
		"uptime":     time.Since(s.deps.Started).Round(time.Second).String(),
		"simulation": s.deps.Sim.Status(),
	})
}

func (s *Server) campusInfo(w http.ResponseWriter, r *http.Request) {
	total, _ := s.deps.State.Group("campus")
	var busy []campus.BuildingID
	for _, b := range s.deps.Campus.Buildings {
		if g, err := s.deps.State.Group(string(b.ID)); err == nil && g.Level >= campus.OccupancyHigh {
			busy = append(busy, b.ID)
		}
	}
	free := 0
	tt, now := s.deps.Timetable(), s.now()
	for _, room := range s.deps.Campus.AllRooms() {
		if s.roomSummary(room, tt, now).Available {
			free++
		}
	}
	trend := s.deps.State.Series("campus.occupancy")["campus.occupancy"]
	s.writeOK(w, r, map[string]any{
		"layout": s.deps.Campus,
		"summary": map[string]any{
			"occupancy":             total,
			"forecast":              s.forecastPoints("campus", "campus"),
			"busy_buildings":        busy,
			"available_study_rooms": free,
			"wifi":                  s.wifiSummary(s.deps.Campus.AllAccessPoints(), s.deps.Campus.AllRooms()),
			"active_alerts":         len(s.deps.State.Alerts(true)),
			"trend":                 trend,
		},
	})
}

func (s *Server) buildings(w http.ResponseWriter, r *http.Request) {
	s.writeOK(w, r, fn.Map(s.deps.Campus.Buildings, s.summarize))
}

func (s *Server) building(w http.ResponseWriter, r *http.Request) {
	b, err := s.deps.Campus.Building(campus.BuildingID(r.PathValue("id")))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	now, tt := s.now(), s.deps.Timetable()
	var upcoming []campus.TimetableEvent
	for _, e := range tt.Between(now, now.Add(3*time.Hour)) {
		if e.BuildingID == b.ID && !e.Cancelled {
			upcoming = append(upcoming, e)
		}
	}
	warnings := fn.Filter(s.deps.State.Warnings(), func(w forecast.ChangeoverWarning) bool { return w.BuildingID == b.ID })
	floors := s.floorViews(b, tt, now)
	recs := finder.Recommend(s.candidates(r.Context(), now), finder.Query{Building: b.ID, Now: now, Limit: 5})
	soon := fn.Filter(warnings, func(w forecast.ChangeoverWarning) bool { return w.EndsAt.Sub(now) <= 2*time.Hour })
	s.writeOK(w, r, map[string]any{
		"summary":     s.summarize(b),
		"floors":      floors,
		"upcoming":    upcoming,
		"warnings":    warnings,
		"study_rooms": recs,
		"brief":       Brief(b, floors, soon, recs, s.summarize(b), now),
	})
}

func (s *Server) floorViews(b *campus.Building, tt Timetable, now time.Time) []FloorView {
	return fn.Map(b.Floors, func(f *campus.Floor) FloorView {
		occ, _ := s.deps.State.Group(string(f.ID))
		fv := FloorView{ID: f.ID, Level: f.Level, Occupancy: occ, Forecast: s.forecastPoints("floor", string(f.ID)),
			WiFi: s.wifiSummary(f.APs, f.Rooms)}
		switch q := forecast.QuietUntil(tt, f, now); {
		case q.Equal(now):
			fv.ClassNow = true
		case !q.IsZero():
			fv.QuietUntil = &q
		}
		for _, room := range append(append([]*campus.Room(nil), f.Rooms...), f.Lobby) {
			fv.Rooms = append(fv.Rooms, s.roomSummary(room, tt, now))
		}
		fv.APs = fn.Map(f.APs, func(a *campus.AccessPoint) campus.AccessPointID { return a.ID })
		return fv
	})
}

func (s *Server) floors(w http.ResponseWriter, r *http.Request) {
	b, err := s.deps.Campus.Building(campus.BuildingID(r.PathValue("id")))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeOK(w, r, s.floorViews(b, s.deps.Timetable(), s.now()))
}

func (s *Server) room(w http.ResponseWriter, r *http.Request) {
	room, err := s.deps.Campus.Room(campus.RoomID(r.PathValue("id")))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	// A forecast is a bonus here: if there is none yet (just after start-up)
	// the room details are still worth returning.
	var fc any
	if f, err := s.deps.Forecaster.Forecast(ctx, campus.ForecastRequest{Subject: string(room.ID), Kind: "room"}); err == nil {
		fc = f
	}
	now, tt := s.now(), s.deps.Timetable()
	st, _ := s.deps.State.Status(room.PrimaryAP)
	q, detail := s.roomQuality(room)
	summary := s.roomSummary(room, tt, now)
	free := campus.OccupancyCount(0)
	if !summary.Estimate.Count.Suppressed {
		free = summary.Estimate.Available()
	}
	s.writeOK(w, r, map[string]any{
		"room":        room,
		"estimate":    summary.Estimate,
		"available":   summary.Available,
		"free_seats":  free,
		"wifi":        q,
		"wifi_detail": detail,
		"ap_status":   st,
		"now":         summary.Now,
		"next":        summary.Next,
		"timetable":   tt.RoomEvents(room.ID),
		"forecast":    fc,
	})
}

// candidates gathers the finder's inputs from live state.
func (s *Server) candidates(ctx context.Context, now time.Time) []finder.Candidate {
	tt := s.deps.Timetable()
	var out []finder.Candidate
	for _, room := range s.deps.Campus.AllRooms() {
		if !room.Type.IsStudyFriendly() {
			continue
		}
		b, _ := s.deps.Campus.Building(room.BuildingID)
		est, _ := s.deps.State.RoomEstimate(room.ID)
		q, detail := s.roomQuality(room)
		c := finder.Candidate{Room: room, Building: b.Name, Estimate: est, WiFi: q, WiFiDetail: detail,
			InClass: len(tt.EventsAt(room.ID, now)) > 0}
		for _, e := range tt.RoomEvents(room.ID) {
			if !e.Cancelled && e.Window.Start.After(now) {
				c.FreeUntil = e.Window.Start
				break
			}
		}
		if f, err := s.deps.Forecaster.Forecast(ctx, campus.ForecastRequest{Subject: string(room.ID), Kind: "room",
			Horizons: []time.Duration{time.Hour}}); err == nil && len(f.Points) == 1 {
			c.Forecast60 = f.Points[0]
		}
		out = append(out, c)
	}
	return out
}

func (s *Server) findRooms(w http.ResponseWriter, r *http.Request) {
	q, err := s.parseQuery(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeOK(w, r, finder.Recommend(s.candidates(r.Context(), q.Now), q))
}

// quietPlace answers "find me a quiet place to study for the next hour".
func (s *Server) quietPlace(w http.ResponseWriter, r *http.Request) {
	minutes, err := queryInt(r, "minutes", 60)
	if err != nil || minutes < 15 || minutes > 480 {
		s.writeError(w, r, fmt.Errorf("%w: minutes must be between 15 and 480", errBadRequest))
		return
	}
	needWiFi := r.URL.Query().Get("wifi") != "optional"
	now := s.now()
	recs := finder.Recommend(s.candidates(r.Context(), now), finder.Query{
		For: time.Duration(minutes) * time.Minute, MaxLevel: campus.OccupancyModerate,
		NeedWiFi: needWiFi, Now: now, Limit: 5,
	})
	s.writeOK(w, r, map[string]any{"minutes": minutes, "wifi_required": needWiFi, "recommendations": recs})
}

func (s *Server) parseQuery(r *http.Request) (finder.Query, error) {
	v := r.URL.Query()
	q := finder.Query{Building: campus.BuildingID(v.Get("building")), Now: s.now(), MaxLevel: campus.OccupancyVeryHigh}
	var err error
	if q.Level, err = queryInt(r, "level", 0); err != nil {
		return q, err
	}
	if q.MinFree, err = queryInt(r, "min_free", 0); err != nil {
		return q, err
	}
	if q.Limit, err = queryInt(r, "limit", 20); err != nil {
		return q, err
	}
	mins, err := queryInt(r, "for", 0)
	if err != nil {
		return q, err
	}
	q.For = time.Duration(mins) * time.Minute
	if t := v.Get("type"); t != "" {
		rt, err := campus.ParseRoomType(t)
		if err != nil {
			return q, err
		}
		q.Type = &rt
	}
	if q.Building != "" {
		if _, err := s.deps.Campus.Building(q.Building); err != nil {
			return q, &campus.ValidationError{Field: "building", Value: q.Building, Reason: "unknown building"}
		}
	}
	switch wq := v.Get("wifi"); wq {
	case "":
	case "reliable":
		q.NeedWiFi = true
	case "excellent", "good", "fair", "poor":
		lvl := map[string]campus.WiFiQuality{"excellent": campus.WiFiExcellent, "good": campus.WiFiGood, "fair": campus.WiFiFair, "poor": campus.WiFiPoor}[wq]
		q.MinWiFi = &lvl
	default:
		return q, &campus.ValidationError{Field: "wifi", Value: wq, Reason: "use reliable, excellent, good, fair or poor"}
	}
	switch lv := v.Get("max_level"); lv {
	case "":
	case "low", "moderate", "high", "very_high":
		q.MaxLevel = map[string]campus.OccupancyLevel{"low": campus.OccupancyLow, "moderate": campus.OccupancyModerate,
			"high": campus.OccupancyHigh, "very_high": campus.OccupancyVeryHigh}[lv]
	default:
		return q, &campus.ValidationError{Field: "max_level", Value: lv, Reason: "use low, moderate, high or very_high"}
	}
	return q, nil
}

func (s *Server) apView(ap *campus.AccessPoint) APView {
	st, _ := s.deps.State.Status(ap.ID)
	return APView{ID: ap.ID, Label: ap.Label, Building: ap.BuildingID, Level: ap.Level, Wing: ap.Wing, Position: ap.Position,
		Channel: ap.Channel, Capacity: ap.MaxClients, Status: st,
		Rooms: fn.Map(s.deps.Campus.RoomsServedBy(ap.ID), func(r *campus.Room) campus.RoomID { return r.ID })}
}

func (s *Server) accessPoints(w http.ResponseWriter, r *http.Request) {
	views := fn.Map(s.deps.Campus.AllAccessPoints(), s.apView)
	if b := r.URL.Query().Get("building"); b != "" {
		views = fn.Filter(views, func(v APView) bool { return string(v.Building) == b })
	}
	if h := r.URL.Query().Get("health"); h != "" {
		views = fn.Filter(views, func(v APView) bool { return v.Status.Health.String() == h })
	}
	s.writeOK(w, r, views)
}

func (s *Server) accessPoint(w http.ResponseWriter, r *http.Request) {
	ap, err := s.deps.Campus.AccessPoint(campus.AccessPointID(r.PathValue("id")))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	v := s.apView(ap)
	hist := s.deps.State.Series("ap."+string(ap.ID)+".latency", "ap."+string(ap.ID)+".utilization")
	s.writeOK(w, r, map[string]any{"access_point": v, "history": hist})
}

func (s *Server) deadZones(w http.ResponseWriter, r *http.Request) {
	views := fn.Filter(fn.Map(s.deps.Campus.AllAccessPoints(), s.apView), func(v APView) bool {
		return slices.ContainsFunc(v.Status.All, func(d campus.Diagnosis) bool { return d.Kind == campus.DiagnosisDeadZone })
	})
	s.writeOK(w, r, views)
}

func (s *Server) alerts(w http.ResponseWriter, r *http.Request) {
	active := r.URL.Query().Get("active") != "false"
	as := s.deps.State.Alerts(active)
	if b := r.URL.Query().Get("building"); b != "" {
		as = fn.Filter(as, func(a campus.Alert) bool { return string(a.BuildingID) == b })
	}
	limit, err := queryInt(r, "limit", 100)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeOK(w, r, fn.Take(as, max(limit, 0)))
}

func (s *Server) forecast(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	subject := r.URL.Query().Get("subject")
	if kind == "" {
		kind, subject = "campus", "campus"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	f, err := s.deps.Forecaster.Forecast(ctx, campus.ForecastRequest{Subject: subject, Kind: kind})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeOK(w, r, map[string]any{"forecast": f, "warnings": s.deps.State.Warnings()})
}

func (s *Server) changeovers(w http.ResponseWriter, r *http.Request) {
	s.writeOK(w, r, s.deps.State.Warnings())
}

// defaultSeries are the charts the dashboard shows.
var defaultSeries = []string{
	"campus.occupancy", "forecast.campus.60", "building.bell.occupancy", "building.seh.occupancy", "building.gelman.occupancy",
	"wifi.avg_latency", "wifi.avg_utilization", "wifi.online_aps", "wifi.problem_aps", "alerts.active",
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	names := defaultSeries
	if q := r.URL.Query().Get("series"); q != "" {
		names = strings.Split(q, ",")
	}
	out := s.deps.State.Series(names...)
	if out == nil {
		out = map[string][]store.Point{}
	}
	s.writeOK(w, r, out)
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	s.writeOK(w, r, s.deps.Metrics())
}

func (s *Server) accuracy(w http.ResponseWriter, r *http.Request) {
	s.writeOK(w, r, s.deps.Accuracy())
}

func (s *Server) simulation(w http.ResponseWriter, r *http.Request) {
	f := s.deps.State.Frame()
	s.writeOK(w, r, map[string]any{"status": s.deps.Sim.Status(), "activity": f.Activity})
}

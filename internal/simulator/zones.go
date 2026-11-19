package simulator

import (
	"math"
	"math/rand/v2"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/campus"
)

// zone simulates the Wi-Fi network of one building. It is the single owner
// of its access points' state (online, failed, recovering), which is the
// "campus zones split across workers, so no locks on shared state" design
// from the CampusPulse proposal.
type zone struct {
	building *campus.Building
	campus   *campus.Campus
	aps      []*campus.AccessPoint
	state    map[campus.AccessPointID]*apState
	rng      *rand.Rand
	failRate float64 // spontaneous failures per AP per simulated hour
}

type apState struct {
	offline   bool
	failedAt  time.Time
	recoverAt time.Time // zero means "until restored by hand"
}

// apCommand asks a zone to fail or restore an AP.
type apCommand struct {
	ap      campus.AccessPointID
	fail    bool
	restore bool
}

type zoneInput struct {
	seq      uint64
	at       time.Time
	step     time.Duration // simulated time since the last tick
	present  []presence    // only this building's rooms
	activity float64
	commands []apCommand
	reset    bool
}

// FaultRecord is ground truth about one AP outage.
type FaultRecord struct {
	AccessPoint campus.AccessPointID `json:"ap_id"`
	Start       time.Time            `json:"start"`
	End         *time.Time           `json:"end,omitempty"`
	Manual      bool                 `json:"manual"`
}

type zoneOutput struct {
	seq       uint64
	building  campus.BuildingID
	obs       []campus.WiFiObservation
	telemetry []campus.APTelemetry
	started   []FaultRecord
	ended     []FaultRecord
	load      map[campus.AccessPointID]float64 // ground truth: associated clients / capacity
}

func newZone(c *campus.Campus, b *campus.Building, seed uint64, failRate float64) *zone {
	z := &zone{building: b, campus: c, state: map[campus.AccessPointID]*apState{}, failRate: failRate,
		rng: rand.New(rand.NewPCG(seed, uint64(len(b.ID))*7919+uint64(b.Floors[0].Elevation)+99))}
	for _, f := range b.Floors {
		for _, ap := range f.APs {
			z.aps = append(z.aps, ap)
			z.state[ap.ID] = &apState{}
		}
	}
	return z
}

func (z *zone) run(in <-chan zoneInput, out chan<- zoneOutput) {
	for inp := range in {
		out <- z.step(inp)
	}
}

func (z *zone) step(in zoneInput) zoneOutput {
	out := zoneOutput{seq: in.seq, building: z.building.ID, load: make(map[campus.AccessPointID]float64, len(z.aps))}
	z.applyCommands(in, &out)
	z.randomFaults(in, &out)

	type apAcc struct {
		clients   int
		signalSum float64
		weak      int
	}
	acc := make(map[campus.AccessPointID]*apAcc, len(z.aps))
	for _, ap := range z.aps {
		acc[ap.ID] = &apAcc{}
	}

	out.obs = make([]campus.WiFiObservation, 0, len(in.present)*2)
	for _, p := range in.present {
		room, err := z.campus.Room(p.room)
		if err != nil {
			continue
		}
		for _, dev := range p.devices {
			if z.rng.Float64() < 0.08 { // asleep or not associated this interval
				continue
			}
			ap, sig, ok := z.associate(room)
			if !ok {
				continue // every nearby AP is down: the device is invisible
			}
			a := acc[ap]
			a.clients++
			a.signalSum += float64(sig)
			if sig < -75 {
				a.weak++
			}
			out.obs = append(out.obs, campus.WiFiObservation{
				RawDeviceID: dev,
				AccessPoint: ap,
				NearestRoom: z.locate(room, ap),
				Signal:      sig,
				Connected:   true,
				At:          in.at,
			})
		}
	}

	for _, ap := range z.aps {
		st := z.state[ap.ID]
		if st.offline {
			continue // failed APs go silent
		}
		a := acc[ap.ID]
		// Printers, displays and other fixed devices on every AP.
		background := 2 + int(math.Round(2*in.activity*z.rng.Float64()))
		clients := a.clients + background
		load := float64(clients) / float64(ap.MaxClients)
		out.load[ap.ID] = load
		util := campus.UtilizationPercentage(4 + 92*math.Pow(load, 1.1)*(0.9+0.2*z.rng.Float64())).Clamp()
		avg := ap.BaseSignal
		weakShare := 0.0
		if a.clients > 0 {
			avg = campus.SignalStrength(a.signalSum / float64(a.clients))
			weakShare = float64(a.weak) / float64(a.clients)
		}
		lat := 5 + 3*math.Exp((float64(util)-55)/11) + z.rng.NormFloat64()
		loss := 0.05 + math.Max(0, float64(util)-80)*0.25 + weakShare*4 + math.Abs(z.rng.NormFloat64())*0.1
		out.telemetry = append(out.telemetry, campus.APTelemetry{
			AccessPoint: ap.ID,
			Clients:     campus.DeviceCount(clients),
			Utilization: util,
			AvgSignal:   avg,
			Latency:     campus.Latency(math.Min(math.Max(lat, 1), 450)),
			PacketLoss:  campus.PacketLoss(math.Min(loss, 60)),
			Online:      true,
			At:          in.at,
		})
	}
	return out
}

// associate picks the AP a device in room connects to and the signal it
// sees. Most devices join the room's own AP; some stick to a neighbour.
// In a dead zone far more devices roam to a neighbour, at a weaker signal.
func (z *zone) associate(room *campus.Room) (campus.AccessPointID, campus.SignalStrength, bool) {
	primary, _ := z.campus.AccessPoint(room.PrimaryAP)
	pNeighbour := 0.15
	if primary.BaseSignal < -80 {
		pNeighbour = 0.45
	}
	order := make([]campus.AccessPointID, 0, 1+len(room.NeighbourAP))
	if z.rng.Float64() < pNeighbour && len(room.NeighbourAP) > 0 {
		order = append(order, room.NeighbourAP[z.rng.IntN(len(room.NeighbourAP))], primary.ID)
	} else {
		order = append(order, primary.ID)
	}
	order = append(order, room.NeighbourAP...)
	for _, id := range order {
		if st := z.state[id]; st == nil || st.offline {
			continue
		}
		ap, _ := z.campus.AccessPoint(id)
		sig := ap.BaseSignal + campus.SignalStrength(z.rng.NormFloat64()*4)
		if id != primary.ID {
			sig = ap.BaseSignal - 16 + campus.SignalStrength(z.rng.NormFloat64()*5)
		}
		// Below about -92 dBm a device cannot hold an association; it tries
		// the next AP instead of reporting an impossible reading.
		if sig < minUsableSignal {
			continue
		}
		return id, sig, true
	}
	return "", 0, false
}

// minUsableSignal is the weakest signal a device stays associated at.
const minUsableSignal campus.SignalStrength = -92

// locate is the controller's guess of which room a device is in. It is
// right most of the time and otherwise picks another space the AP covers.
func (z *zone) locate(room *campus.Room, ap campus.AccessPointID) campus.RoomID {
	if z.rng.Float64() < 0.85 {
		return room.ID
	}
	served := z.campus.RoomsServedBy(ap)
	if len(served) == 0 {
		return room.ID
	}
	return served[z.rng.IntN(len(served))].ID
}

func (z *zone) applyCommands(in zoneInput, out *zoneOutput) {
	if in.reset {
		for id, st := range z.state {
			if st.offline {
				end := in.at
				out.ended = append(out.ended, FaultRecord{AccessPoint: id, Start: st.failedAt, End: &end})
			}
			*st = apState{}
		}
	}
	for _, c := range in.commands {
		st, ok := z.state[c.ap]
		if !ok {
			continue
		}
		switch {
		case c.fail && !st.offline:
			*st = apState{offline: true, failedAt: in.at}
			out.started = append(out.started, FaultRecord{AccessPoint: c.ap, Start: in.at, Manual: true})
		case c.restore && st.offline:
			end := in.at
			out.ended = append(out.ended, FaultRecord{AccessPoint: c.ap, Start: st.failedAt, End: &end, Manual: true})
			*st = apState{}
		}
	}
}

// randomFaults fails and recovers APs on their own, so IT mode has something
// to find even when nobody presses the "fail AP" button.
func (z *zone) randomFaults(in zoneInput, out *zoneOutput) {
	hours := in.step.Hours()
	for _, ap := range z.aps {
		st := z.state[ap.ID]
		switch {
		case st.offline && !st.recoverAt.IsZero() && !in.at.Before(st.recoverAt):
			end := in.at
			out.ended = append(out.ended, FaultRecord{AccessPoint: ap.ID, Start: st.failedAt, End: &end})
			*st = apState{}
		case !st.offline && hours > 0 && z.rng.Float64() < z.failRate*hours:
			rec := in.at.Add(time.Duration(20+z.rng.IntN(30)) * time.Minute)
			*st = apState{offline: true, failedAt: in.at, recoverAt: rec}
			out.started = append(out.started, FaultRecord{AccessPoint: ap.ID, Start: in.at})
		}
	}
}

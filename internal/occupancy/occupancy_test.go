package occupancy

import (
	"testing"
	"time"

	"github.com/dylanpanashe/csci_6221/internal/campus"
	"github.com/dylanpanashe/csci_6221/internal/privacy"
)

func TestEstimateDevicePopulation(t *testing.T) {
	tests := []struct {
		devices campus.DeviceCount
		factor  float64
		want    float64
	}{
		{165, 1.65, 100}, {0, 1.65, 0}, {10, 0, 0}, {20, 2, 10},
	}
	for _, tt := range tests {
		if got := EstimateDevicePopulation(tt.devices, tt.factor); got != tt.want {
			t.Errorf("EstimateDevicePopulation(%d, %v) = %v, want %v", tt.devices, tt.factor, got, tt.want)
		}
	}
}

func TestCalculateOccupancy(t *testing.T) {
	w := Weights{WiFiDuringClass: 0.75, Smoothing: 1}
	tests := []struct {
		name    string
		in      FusionInput
		people  float64
		minConf float64
		maxConf float64
	}{
		{"no class: Wi-Fi only", FusionInput{WiFiPeople: 40, APOnline: true}, 40, 0.79, 0.81},
		{"class: blended", FusionInput{WiFiPeople: 200, Expected: 240, ClassRunning: true, APOnline: true}, 210, 0.7, 0.9},
		{"class cancelled, timetable wrong", FusionInput{WiFiPeople: 5, Expected: 100, ClassRunning: true, APOnline: true}, 28.75, 0.45, 0.6},
		{"AP down during class: timetable", FusionInput{WiFiPeople: 0, Expected: 80, ClassRunning: true}, 80, 0.3, 0.4},
		{"AP down, no class: low confidence", FusionInput{WiFiPeople: 12}, 12, 0.2, 0.3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := CalculateOccupancy(tt.in, w)
			if diff := r.People - tt.people; diff > 0.01 || diff < -0.01 {
				t.Errorf("people = %v, want %v", r.People, tt.people)
			}
			if r.Confidence < tt.minConf || r.Confidence > tt.maxConf {
				t.Errorf("confidence = %v, want %v-%v", r.Confidence, tt.minConf, tt.maxConf)
			}
		})
	}
}

func TestCalculateRoomAvailability(t *testing.T) {
	free, ratio := CalculateRoomAvailability(30, 40)
	if free != 10 || ratio != 0.75 {
		t.Fatalf("free=%d ratio=%v", free, ratio)
	}
	if free, _ := CalculateRoomAvailability(55, 40); free != 0 {
		t.Fatalf("over capacity should give 0 free seats, got %d", free)
	}
	if free, ratio := CalculateRoomAvailability(5, 0); free != 0 || ratio != 0 {
		t.Fatal("zero capacity")
	}
}

func TestFusionResetsSmoothingAtClassBoundaries(t *testing.T) {
	c := campus.NewGWUCampus()
	r, _ := c.Room("bell-101")
	f := NewFusion(Weights{WiFiDuringClass: 1, Smoothing: 0.5}, nil)
	at := time.Date(2026, 11, 18, 13, 0, 0, 0, time.UTC)
	f.Estimate(campus.EstimateInput{Room: r, Devices: 0, DevicesPerUser: 1, At: at})
	ev := campus.TimetableEvent{RoomID: r.ID, ExpectedAttendance: 250, Window: campus.Window{Start: at, End: at.Add(time.Hour)}}
	e := f.Estimate(campus.EstimateInput{Room: r, Devices: 280, ActiveEvents: []campus.TimetableEvent{ev}, DevicesPerUser: 1, At: at.Add(time.Minute)})
	if e.Count.Value != 280 {
		t.Fatalf("estimate = %d; smoothing should restart when a class begins", e.Count.Value)
	}
}

func TestPublishSuppressesSmallRooms(t *testing.T) {
	p := privacy.NewPublisher(10, 0, 1)
	small := Publish(campus.OccupancyEstimate{Count: campus.PublicCount{Value: 4}, Low: 3, High: 6, Capacity: 6, Ratio: 0.66,
		Devices: campus.PublicCount{Value: 7}}, p)
	if !small.Count.Suppressed || small.Count.Display != "<10" || small.Low != 0 || small.High != 0 {
		t.Fatalf("small room leaked: %+v", small)
	}
	// 7 devices / 1.65 or ratio x capacity would reveal the hidden count.
	if !small.Devices.Suppressed || small.Devices.Value != 0 || small.Ratio != 0 {
		t.Fatalf("suppressed count recoverable from devices or ratio: %+v", small)
	}
	// Even 15 devices must be hidden when the people count is suppressed.
	leaky := Publish(campus.OccupancyEstimate{Count: campus.PublicCount{Value: 8}, Capacity: 16, Ratio: 0.5,
		Devices: campus.PublicCount{Value: 15}}, p)
	if !leaky.Devices.Suppressed || leaky.Devices.Display != "<10" {
		t.Fatalf("device count published for a suppressed room: %+v", leaky.Devices)
	}
	big := Publish(campus.OccupancyEstimate{Count: campus.PublicCount{Value: 120}, Capacity: 300}, p)
	if big.Count.Suppressed || big.Count.Value != 120 {
		t.Fatalf("large count: %+v", big.Count)
	}
}

func TestAggregateThenPublishKeepsGroupsUseful(t *testing.T) {
	rooms := []campus.OccupancyEstimate{
		{Count: campus.PublicCount{Value: 4}, Capacity: 6}, {Count: campus.PublicCount{Value: 5}, Capacity: 6},
		{Count: campus.PublicCount{Value: 6}, Capacity: 10},
	}
	g := Publish(Aggregate("gelman-3", rooms), privacy.NewPublisher(10, 0, 1))
	if g.Count.Suppressed || g.Count.Value != 15 || g.Capacity != 22 {
		t.Fatalf("group = %+v", g)
	}
}

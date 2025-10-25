package hetzner

import (
	"testing"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

func TestNew(t *testing.T) {
	// Test that New creates a client without panicking
	token := "test-token"
	client := New(token)

	if client == nil {
		t.Fatal("New() returned nil client")
	}

	if client.hcloud == nil {
		t.Error("New() did not initialize hcloud client")
	}
}

func TestGetNetworkZoneFromLocation(t *testing.T) {
	tests := []struct {
		name     string
		location string
		want     hcloud.NetworkZone
	}{
		{
			name:     "Falkenstein - EU Central",
			location: "fsn1",
			want:     hcloud.NetworkZoneEUCentral,
		},
		{
			name:     "Nuremberg - EU Central",
			location: "nbg1",
			want:     hcloud.NetworkZoneEUCentral,
		},
		{
			name:     "Helsinki - EU Central",
			location: "hel1",
			want:     hcloud.NetworkZoneEUCentral,
		},
		{
			name:     "Ashburn - US East",
			location: "ash",
			want:     hcloud.NetworkZoneUSEast,
		},
		{
			name:     "Hillsboro - US West",
			location: "hil",
			want:     hcloud.NetworkZoneUSWest,
		},
		{
			name:     "Unknown location defaults to EU Central",
			location: "unknown",
			want:     hcloud.NetworkZoneEUCentral,
		},
		{
			name:     "Empty location defaults to EU Central",
			location: "",
			want:     hcloud.NetworkZoneEUCentral,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getNetworkZoneFromLocation(tt.location)
			if got != tt.want {
				t.Errorf("getNetworkZoneFromLocation(%s) = %v, want %v", tt.location, got, tt.want)
			}
		})
	}
}

func TestGetNetworkZoneFromLocationCoverage(t *testing.T) {
	// Verify all documented locations are covered
	locations := []string{"fsn1", "nbg1", "hel1", "ash", "hil"}

	for _, location := range locations {
		zone := getNetworkZoneFromLocation(location)
		if zone == "" {
			t.Errorf("getNetworkZoneFromLocation(%s) returned empty zone", location)
		}
	}
}

func TestGetNetworkZoneFromLocationMapping(t *testing.T) {
	// Test that EU locations map to EU zone
	euLocations := []string{"fsn1", "nbg1", "hel1"}
	for _, location := range euLocations {
		zone := getNetworkZoneFromLocation(location)
		if zone != hcloud.NetworkZoneEUCentral {
			t.Errorf("EU location %s mapped to %v, want %v", location, zone, hcloud.NetworkZoneEUCentral)
		}
	}

	// Test that US locations map to appropriate US zones
	ashZone := getNetworkZoneFromLocation("ash")
	if ashZone != hcloud.NetworkZoneUSEast {
		t.Errorf("ash location mapped to %v, want %v", ashZone, hcloud.NetworkZoneUSEast)
	}

	hilZone := getNetworkZoneFromLocation("hil")
	if hilZone != hcloud.NetworkZoneUSWest {
		t.Errorf("hil location mapped to %v, want %v", hilZone, hcloud.NetworkZoneUSWest)
	}
}

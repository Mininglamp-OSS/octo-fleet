package runtime

import (
	"testing"
)

func TestIsAllowedGatewayURL(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		expected bool
	}{
		{"https allowed", "https://api.example.com/v1", true},
		{"https no path", "https://example.com", true},
		{"http localhost", "http://localhost:8080", true},
		{"http 127.0.0.1", "http://127.0.0.1:3000/api", true},
		{"http localhost no port", "http://localhost", true},
		{"http example rejected", "http://example.com", false},
		{"http remote rejected", "http://192.168.1.1:8080", false},
		{"ftp rejected", "ftp://files.example.com", false},
		{"empty rejected", "", false},
		{"spaces only", "   ", false},
		{"garbage rejected", "not-a-url", false},
		{"no scheme rejected", "example.com", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isAllowedGatewayURL(tt.url)
			if result != tt.expected {
				t.Errorf("isAllowedGatewayURL(%q) = %v, want %v", tt.url, result, tt.expected)
			}
		})
	}
}

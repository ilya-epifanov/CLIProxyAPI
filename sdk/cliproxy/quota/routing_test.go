package quota

import (
	"reflect"
	"testing"
)

func TestRoutingWindowsFor(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		model    string
		want     RoutingWindows
		wantOK   bool
	}{
		{"claude non-Fable", "claude", "claude-sonnet-4-5", RoutingWindows{Short: FiveHour, Long: []Slot{{SevenDay}}}, true},
		{"claude Fable", " Claude ", "claude-fable-5", RoutingWindows{Short: FiveHour, Long: []Slot{{SevenDay}, {SevenDayFable}}}, true},
		{"claude Fable with suffix", "claude", " CLAUDE-FABLE-5-1(high)", RoutingWindows{Short: FiveHour, Long: []Slot{{SevenDay}, {SevenDayFable}}}, true},
		{"codex", "CODEX", "gpt-5-codex", RoutingWindows{Short: FiveHour, Long: []Slot{{Weekly, Monthly}}}, true},
		{"unknown provider", "gemini", "claude-fable-5", RoutingWindows{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := RoutingWindowsFor(tt.provider, tt.model)
			if ok != tt.wantOK || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("RoutingWindowsFor(%q, %q) = %+v, %v; want %+v, %v", tt.provider, tt.model, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestIsFableModel(t *testing.T) {
	tests := map[string]bool{
		"claude-fable-5":           true,
		"  Claude-Fable-5-1(high)": true,
		"claude-opus-4-1":          false,
		"fable":                    false,
		"":                         false,
	}
	for model, want := range tests {
		if got := IsFableModel(model); got != want {
			t.Errorf("IsFableModel(%q) = %v, want %v", model, got, want)
		}
	}
}

package platform

import "testing"

func TestIsPodInstanceID(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{id: "cpod-abc", want: true},
		{id: " CPOD-ABC ", want: true},
		{id: "uhost-abc", want: false},
		{id: "", want: false},
	} {
		if got := IsPodInstanceID(tc.id); got != tc.want {
			t.Fatalf("IsPodInstanceID(%q)=%v, want %v", tc.id, got, tc.want)
		}
	}
}

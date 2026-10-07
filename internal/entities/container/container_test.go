package container

import "testing"

func TestIsIdleManaged(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{"sablier enabled", map[string]string{"sablier.enable": "true"}, true},
		{"sablier enabled capitalized", map[string]string{"sablier.enable": "True"}, true},
		{"sablier enabled as 1", map[string]string{"sablier.enable": "1"}, true},
		{"our label", map[string]string{"closeyourit.idle_managed": "true"}, true},
		{"sablier disabled", map[string]string{"sablier.enable": "false"}, false},
		{"unexpected value", map[string]string{"sablier.enable": "yes"}, false},
		{"empty value", map[string]string{"sablier.enable": ""}, false},
		{"other labels", map[string]string{"sablier.group": "acme"}, false},
		{"no labels", map[string]string{}, false},
		{"labels nil", nil, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsIdleManaged(tc.labels); got != tc.want {
				t.Errorf("IsIdleManaged(%v) = %v, expected %v", tc.labels, got, tc.want)
			}
		})
	}
}

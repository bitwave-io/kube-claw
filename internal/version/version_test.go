package version

import "testing"

func TestNewer(t *testing.T) {
	cases := []struct {
		candidate, current string
		want               bool
	}{
		{"v0.4.0", "v0.3.1", true},
		{"0.4.0", "0.3.1", true}, // missing v canonicalized
		{"v0.3.1", "v0.3.1", false},
		{"v0.3.0", "v0.3.1", false},
		{"v1.0.0", "v0.99.9", true},
		{"v0.4.0", "dev", false},   // dev build: never upgradeable (fail closed)
		{"garbage", "v0.3.1", false},
		{"", "v0.3.1", false},
	}
	for _, c := range cases {
		if got := Newer(c.candidate, c.current); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.candidate, c.current, got, c.want)
		}
	}
}

func TestMax(t *testing.T) {
	cases := []struct{ a, b, want string }{
		{"v0.3.1", "v0.4.0", "v0.4.0"},
		{"v0.4.0", "v0.3.1", "v0.4.0"},
		{"v0.4.0", "", "v0.4.0"},
		{"", "v0.4.0", "v0.4.0"},
		{"v0.4.0", "garbage", "v0.4.0"},
		{"dev", "v0.4.0", "v0.4.0"},
		{"dev", "junk", "dev"}, // two invalids: first wins
	}
	for _, c := range cases {
		if got := Max(c.a, c.b); got != c.want {
			t.Errorf("Max(%q, %q) = %q, want %q", c.a, c.b, got, c.want)
		}
	}
}

func TestGetCanonicalizes(t *testing.T) {
	old := Version
	defer func() { Version = old }()
	Version = "0.4.0"
	if got := Get(); got != "v0.4.0" {
		t.Errorf("Get() = %q, want v0.4.0", got)
	}
	Version = "dev"
	if got := Get(); got != "dev" {
		t.Errorf("Get() = %q, want dev", got)
	}
	if IsRelease() {
		t.Error("IsRelease() = true for dev build")
	}
}

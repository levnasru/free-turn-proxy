package udprelay

import "testing"

func TestGroupPrefix24(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		hostport string
		want    string
	}{
		{"ipv4 with port", "203.0.113.42:3478", "203.0.113"},
		{"ipv4 different last octet still same group", "203.0.113.7:443", "203.0.113"},
		{"ipv4 different /24", "203.0.114.7:3478", "203.0.114"},
		{"no port falls back to raw host", "203.0.113.42", "203.0.113.42"},
		{"unresolved hostname falls back to itself", "relay.example.internal:3478", "relay.example.internal:3478"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := groupPrefix24(c.hostport); got != c.want {
				t.Errorf("groupPrefix24(%q) = %q, want %q", c.hostport, got, c.want)
			}
		})
	}
}

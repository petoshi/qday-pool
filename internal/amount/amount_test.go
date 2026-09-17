package amount

import "testing"

func TestParse(t *testing.T) {
	for _, test := range []struct {
		display, unit, want string
	}{
		{"1", "1000000", "1000000"},
		{"0.001", "1000000", "1000"},
		{"12.3405", "1000000", "12340500"},
		{"0", "1", "0"},
	} {
		got, err := Parse(test.display, test.unit)
		if err != nil || got.String() != test.want {
			t.Fatalf("Parse(%q, %q) = %v, %v; want %s", test.display, test.unit, got, err, test.want)
		}
	}
	for _, value := range []string{"", "-1", "+1", "01", "1.", ".1", "1.0000001", "nan"} {
		if _, err := Parse(value, "1000000"); err == nil {
			t.Fatalf("accepted invalid amount %q", value)
		}
	}
}

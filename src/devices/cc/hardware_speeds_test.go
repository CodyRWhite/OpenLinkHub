package cc

import "testing"

func TestParseHwModes(t *testing.T) {
	modes, err := parseHwModes([]byte{0x07, 0x02, 0x02, 0x00, 0x02, 0x02, 0x02, 0x02, 0xff})
	if err != nil || len(modes) != 7 || modes[2] != 0x00 {
		t.Fatalf("got %x, %v", modes, err)
	}
	for _, bad := range [][]byte{{}, {0x00}, {0x07, 0x02}, {0x20}} {
		if _, err := parseHwModes(bad); err == nil {
			t.Errorf("expected error for %x", bad)
		}
	}
}

func TestParseHwFixed(t *testing.T) {
	values, err := parseHwFixed([]byte{0x02, 0x4b, 0x00, 0x28, 0x00})
	if err != nil || len(values) != 2 || values[0] != 75 || values[1] != 40 {
		t.Fatalf("got %v, %v", values, err)
	}
	for _, bad := range [][]byte{{}, {0x02, 0x4b, 0x00}, {0x01, 0x65, 0x00}} {
		if _, err := parseHwFixed(bad); err == nil {
			t.Errorf("expected error for %x", bad)
		}
	}
}

func TestParseHwCurves(t *testing.T) {
	// Two channels: liquid sensor, 2 points (25.0C 30%, 40.0C 100%); probe sensor, 0 points.
	data := []byte{0x02, 0x00, 0x02, 0xfa, 0x00, 0x1e, 0x00, 0x90, 0x01, 0x64, 0x00, 0x01, 0x00}
	curves, err := parseHwCurves(data)
	if err != nil || len(curves) != 2 || curves[0].Points[1] != [2]int{400, 100} || curves[1].Sensor != 0x01 {
		t.Fatalf("got %+v, %v", curves, err)
	}
	for _, bad := range [][]byte{{}, {0x01, 0x00, 0x08}, {0x01, 0x00, 0x02, 0xfa}} {
		if _, err := parseHwCurves(bad); err == nil {
			t.Errorf("expected error for %x", bad)
		}
	}
}

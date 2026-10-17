package pg

import "testing"

func TestParseDF(t *testing.T) {
	out := "Filesystem     1024-blocks     Used Available Capacity Mounted on\n/dev/vda1         61255492 20971520  37143244      37% /\n"
	n, err := ParseDF([]byte(out))
	if err != nil || n != 37143244*1024 {
		t.Errorf("%d %v", n, err)
	}
	if _, err := ParseDF([]byte("garbage")); err == nil {
		t.Error("garbage must fail")
	}
}

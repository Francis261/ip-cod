package protocol

import "testing"

func TestEffectivePorts(t *testing.T) {
	cases := []struct {
		name string
		p    IPChangedPayload
		want []uint16
	}{
		{"multi-port", IPChangedPayload{Port: 22, Ports: []uint16{22, 443}}, []uint16{22, 443}},
		{"legacy single port", IPChangedPayload{Port: 8080}, []uint16{8080}},
		{"ports wins over port", IPChangedPayload{Port: 1, Ports: []uint16{9}}, []uint16{9}},
		{"empty", IPChangedPayload{}, nil},
	}
	for _, tc := range cases {
		got := tc.p.EffectivePorts()
		if len(got) != len(tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
			}
		}
	}
}

func TestIPChangedRoundTrip(t *testing.T) {
	msg := &Message{
		Type: TypeIPChanged,
		IPChanged: &IPChangedPayload{
			IP:       "1.2.3.4",
			Port:     22,
			Ports:    []uint16{22, 443},
			Proto:    "tcp",
			Revision: 7,
		},
	}
	data, err := Encode(msg)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Type != TypeIPChanged || got.IPChanged == nil {
		t.Fatalf("round trip lost payload: %+v", got)
	}
	p := got.IPChanged
	if p.IP != "1.2.3.4" || p.Port != 22 || p.Proto != "tcp" || p.Revision != 7 {
		t.Fatalf("payload mismatch: %+v", p)
	}
	ports := p.EffectivePorts()
	if len(ports) != 2 || ports[0] != 22 || ports[1] != 443 {
		t.Fatalf("ports mismatch: %v", ports)
	}
}

func TestDecodeLegacySinglePortPayload(t *testing.T) {
	// A frame from a pre-multi-port coordinator: only "port", no "ports".
	data := []byte(`{"type":"ip_changed","ip_changed":{"ip":"5.6.7.8","port":2222,"revision":3}}`)
	m, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	ports := m.IPChanged.EffectivePorts()
	if len(ports) != 1 || ports[0] != 2222 {
		t.Fatalf("legacy fallback failed: %v", ports)
	}
}

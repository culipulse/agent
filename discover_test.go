package main

import "testing"

func TestExpandCIDR(t *testing.T) {
	h, err := expandCIDR("127.0.0.1/32")
	if err != nil || len(h) != 1 || h[0] != "127.0.0.1" {
		t.Errorf("/32: %v %v", h, err)
	}
	if h, _ := expandCIDR("10.0.0.5"); len(h) != 1 {
		t.Errorf("bare ip: %v", h)
	}
	if h, err := expandCIDR("192.168.1.0/30"); err != nil || len(h) != 4 {
		t.Errorf("/30: %v %v", h, err)
	}
	if _, err := expandCIDR("10.0.0.0/8"); err == nil {
		t.Error("expected too-wide rejection")
	}
	if _, err := expandCIDR("not-a-cidr!!"); err == nil {
		t.Error("expected parse error")
	}
}

func TestGuessService(t *testing.T) {
	if guessService(443) != "https" || guessService(22) != "ssh" || guessService(12345) != "tcp" {
		t.Error("guessService map wrong")
	}
}

func TestParsePorts(t *testing.T) {
	if got := parsePorts("80,443, 22"); len(got) != 3 {
		t.Errorf("explicit: %v", got)
	}
	if got := parsePorts(""); len(got) < 5 {
		t.Errorf("default: %v", got)
	}
	if got := parsePorts("0,99999,abc,80"); len(got) != 1 || got[0] != 80 {
		t.Errorf("filter invalid: %v", got)
	}
}

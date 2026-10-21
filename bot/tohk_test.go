package bot

import (
	"testing"

	"github.com/automuteus/automuteus/v8/bot/command"
)

func TestTohkTrusted(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:5555":   true,
		"10.200.0.1:1234":  true,
		"172.18.0.1:1":     true,
		"192.168.1.14:80":  true,
		"100.108.25.117:9": true,
		"[::1]:5000":       true,
		"203.0.113.5:443":  false,
		"8.8.8.8:53":       false,
		"not-an-address":   false,
		"100.128.0.1:1":    false,
	} {
		if got := tohkTrusted(addr); got != want {
			t.Errorf("%s: got %v, want %v", addr, got, want)
		}
	}
}

func TestTohkValidate(t *testing.T) {
	ok := tohkResult{Schema: 1, ID: "abc", EndedAt: 1, Players: []tohkPlayer{{ID: 0}}}
	if err := ok.validate(); err != nil {
		t.Fatal(err)
	}
	bad := ok
	bad.Schema = 2
	if bad.validate() == nil {
		t.Error("schema 2 accepted")
	}
	bad = ok
	bad.Players = nil
	if bad.validate() == nil {
		t.Error("no players accepted")
	}
}

func TestTohkCommandRegistered(t *testing.T) {
	n := 0
	for _, c := range command.All {
		if c.Name == "tohk" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("tohk registered %d times", n)
	}
}

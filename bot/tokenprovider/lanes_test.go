package tokenprovider

import (
	"testing"

	"github.com/bwmarrin/discordgo"
)

// fork: 作業用 bot (名前順・上限まで) の後ろに本体 bot が 1 本並ぶ
func TestMuteLanes(t *testing.T) {
	tp := &TokenProvider{
		primarySession: &discordgo.Session{},
		activeSessions: map[string]*discordgo.Session{"b": {}, "a": {}, "c": {}},
	}
	lanes := tp.muteLanes(2)
	if len(lanes) != 3 || lanes[0].hToken != "a" || lanes[1].hToken != "b" || lanes[2].hToken != "" || lanes[2].sess != tp.primarySession {
		t.Fatalf("unexpected lanes: %+v", lanes)
	}
	if got := (&TokenProvider{activeSessions: map[string]*discordgo.Session{}}).muteLanes(5); len(got) != 0 {
		t.Fatalf("expected no lanes, got %d", len(got))
	}
}

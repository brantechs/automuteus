package bot

import (
	"encoding/json"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func parseStatus(t *testing.T, raw string) *autohostStatus {
	t.Helper()
	var st autohostStatus
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		t.Fatal(err)
	}
	return &st
}

func disabledByID(row discordgo.ActionsRow) map[string]bool {
	out := map[string]bool{}
	for _, c := range row.Components {
		b := c.(discordgo.Button)
		out[b.CustomID] = b.Disabled
	}
	return out
}

func TestAutohostButtons(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want map[string]bool // CustomID -> disabled
	}{
		{"no status", "", map[string]bool{autohostBtnStart: true, autohostBtnGameStart: true, autohostBtnRecreate: true, autohostBtnStop: true}},
		{"stopped", `{"agent_ok":true,"host":{"game_running":false},"lobby":{"state":"stopped"}}`,
			map[string]bool{autohostBtnStart: false, autohostBtnGameStart: true, autohostBtnRecreate: true, autohostBtnStop: true}},
		{"starting", `{"busy":"starting","host":{"game_running":false},"lobby":{}}`,
			map[string]bool{autohostBtnStart: true, autohostBtnGameStart: true, autohostBtnRecreate: true, autohostBtnStop: true}},
		{"lobby", `{"agent_ok":true,"host":{"game_running":true},"lobby":{"state":"lobby","code":"ECVRTG"}}`,
			map[string]bool{autohostBtnStart: true, autohostBtnGameStart: false, autohostBtnRecreate: false, autohostBtnStop: false}},
		{"menu", `{"agent_ok":true,"host":{"game_running":true},"lobby":{"state":"menu"}}`,
			map[string]bool{autohostBtnStart: false, autohostBtnGameStart: true, autohostBtnRecreate: true, autohostBtnStop: false}},
		{"ingame", `{"agent_ok":true,"host":{"game_running":true},"lobby":{"state":"ingame"}}`,
			map[string]bool{autohostBtnStart: true, autohostBtnGameStart: true, autohostBtnRecreate: true, autohostBtnStop: false}},
		{"other guild", `{"agent_ok":true,"host":{"game_running":true},"lobby":{"state":"lobby"},"link":{"guild_id":"999","connect_code":"AAAAAAAA"}}`,
			map[string]bool{autohostBtnStart: true, autohostBtnGameStart: true, autohostBtnRecreate: true, autohostBtnStop: true}},
	}
	for _, c := range cases {
		var st *autohostStatus
		if c.raw != "" {
			st = parseStatus(t, c.raw)
		}
		got := disabledByID(autohostComponents(st, "111"))
		for id, want := range c.want {
			if got[id] != want {
				t.Errorf("%s: %s disabled=%v, want %v", c.name, id, got[id], want)
			}
		}
	}
}

func TestAutohostIDSet(t *testing.T) {
	s := autohostIDSet(" 1, 2 ,,3")
	if len(s) != 3 || !s["1"] || !s["2"] || !s["3"] {
		t.Fatalf("unexpected set %v", s)
	}
}

func TestAutohostEnabledFor(t *testing.T) {
	oldEnabled, oldGuilds := autohostEnabled, autohostGuilds
	defer func() { autohostEnabled, autohostGuilds = oldEnabled, oldGuilds }()

	autohostEnabled, autohostGuilds = false, map[string]bool{}
	if autohostEnabledFor("1") {
		t.Fatal("disabled globally")
	}
	autohostEnabled = true
	if !autohostEnabledFor("1") {
		t.Fatal("empty guild list means all guilds")
	}
	autohostGuilds = map[string]bool{"2": true}
	if autohostEnabledFor("1") || !autohostEnabledFor("2") {
		t.Fatal("guild allow list not honored")
	}
}

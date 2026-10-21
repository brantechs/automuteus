package bot

import (
	"encoding/json"
	"strings"
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
		{"no status", "", map[string]bool{autohostBtnStart: true, autohostBtnGameStart: true, autohostBtnStop: false}},
		{"stopped", `{"agent_ok":true,"host":{"game_running":false},"lobby":{"state":"stopped"}}`,
			map[string]bool{autohostBtnStart: false, autohostBtnGameStart: true, autohostBtnStop: false}},
		{"agent down", `{"agent_ok":false,"host":{"game_running":false},"lobby":{"state":"stopped"}}`,
			map[string]bool{autohostBtnGameStart: true, autohostBtnStop: false}},
		{"starting", `{"busy":"starting","host":{"game_running":false},"lobby":{}}`,
			map[string]bool{autohostBtnStart: true, autohostBtnGameStart: true, autohostBtnStop: true}},
		// ロビーでは部屋ボタンが「部屋を立て直す」になって押せる
		{"lobby", `{"agent_ok":true,"host":{"game_running":true},"lobby":{"state":"lobby","code":"ECVRTG"}}`,
			map[string]bool{autohostBtnStart: false, autohostBtnGameStart: false, autohostBtnStop: false}},
		// 部屋ガチャ中 (announced=false) は AutoLobby が自分で立て直しているので、部屋ボタンも試合開始も押せない
		{"rolling", `{"agent_ok":true,"host":{"game_running":true},"lobby":{"state":"lobby","code":"CSSULG","announced":false}}`,
			map[string]bool{autohostBtnStart: true, autohostBtnGameStart: true, autohostBtnStop: false}},
		{"menu", `{"agent_ok":true,"host":{"game_running":true},"lobby":{"state":"menu"}}`,
			map[string]bool{autohostBtnStart: false, autohostBtnGameStart: true, autohostBtnStop: false}},
		{"ingame", `{"agent_ok":true,"host":{"game_running":true},"lobby":{"state":"ingame"}}`,
			map[string]bool{autohostBtnStart: true, autohostBtnGameStart: true, autohostBtnStop: false}},
		{"other guild", `{"agent_ok":true,"host":{"game_running":true},"lobby":{"state":"lobby"},"link":{"guild_id":"999","connect_code":"AAAAAAAA"}}`,
			map[string]bool{autohostBtnStart: true, autohostBtnGameStart: true, autohostBtnStop: false}},
		{"stopping", `{"busy":"stopping","agent_ok":true,"host":{"game_running":true},"lobby":{"state":"lobby"}}`,
			map[string]bool{autohostBtnStart: true, autohostBtnGameStart: true, autohostBtnStop: true}},
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

func TestAutohostRoomButtonLabel(t *testing.T) {
	labels := func(raw string) []string {
		var out []string
		for _, c := range autohostComponents(parseStatus(t, raw), "111").Components {
			out = append(out, c.(discordgo.Button).Label)
		}
		return out
	}
	if got := labels(`{"agent_ok":true,"host":{"game_running":true},"lobby":{"state":"lobby"}}`); got[0] != "試合開始" || got[1] != "部屋を立て直す" || len(got) != 3 {
		t.Fatalf("lobby: %v", got)
	}
	if got := labels(`{"agent_ok":true,"host":{"game_running":false},"lobby":{"state":"stopped"}}`); got[1] != "部屋を立てる" || got[2] != "ホスト終了" {
		t.Fatalf("stopped: %v", got)
	}
}

// discordgo v0.27.1 は Button.Emoji が空でも "emoji":{} を送り、Discord が COMPONENT_INVALID_EMOJI で弾く
func TestAutohostButtonsHaveEmoji(t *testing.T) {
	for _, c := range autohostComponents(nil, "111").Components {
		b := c.(discordgo.Button)
		if b.Emoji.Name == "" && b.Emoji.ID == "" {
			t.Errorf("button %s has no emoji", b.CustomID)
		}
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) == "" || json.Valid(raw) == false {
			t.Fatalf("invalid json for %s", b.CustomID)
		}
	}
}

func TestAutohostStatusAcceptsFloats(t *testing.T) {
	st := parseStatus(t, `{"agent_ok":true,"host":{"game_running":true},"lobby":{"state":"lobby","code":"ECVRTG","ping":4.0,"players":3,"recreates":1.0},
		"regulation":{"name":"10人","preset":1.0,"roles":[{"name":"インポスター","count":2.0,"chance":100}]}}`)
	if st.Lobby.Ping == nil || *st.Lobby.Ping != 4 || st.Regulation.Roles[0].Count != 2 {
		t.Fatalf("unexpected parse: %+v", st)
	}
}

func TestAutohostStateLine(t *testing.T) {
	cases := map[string]string{
		`{"busy":"starting","host":{},"lobby":{}}`:                                                                           "起動中",
		`{"agent_ok":true,"host":{"game_running":true},"lobby":{"state":"signing-in"}}`:                                      "サインイン",
		`{"agent_ok":true,"host":{"game_running":true},"lobby":{"state":"booting"}}`:                                         "TOH-K を起動",
		`{"agent_ok":true,"host":{"game_running":true},"lobby":{"state":"lobby","announced":false,"recreates":2,"ping":49}}`: "3部屋目",
		`{"agent_ok":true,"host":{"game_running":true},"lobby":{"state":"lobby","announced":true}}`:                          "ロビーで待機中",
		`{"agent_ok":true,"host":{"game_running":false},"lobby":{"state":"stopped"}}`:                                        "停止中",
	}
	for raw, want := range cases {
		if got := autohostStateLine(parseStatus(t, raw), "111"); !strings.Contains(got, want) {
			t.Errorf("%s: got %q, want contains %q", raw, got, want)
		}
	}
}

func TestConnectionPayloadLinked(t *testing.T) {
	cases := map[interface{}]bool{
		"true": true, `{"connected":true}`: true, "false": false, `{"connected":false}`: false, "": false, 1: false,
	}
	for p, want := range cases {
		if got := connectionPayloadLinked(p); got != want {
			t.Errorf("%v: got %v want %v", p, got, want)
		}
	}
}

func TestAutohostRecreateRateLimited(t *testing.T) {
	if !autohostRecreateAllowed("g-test") {
		t.Fatal("first recreate should be allowed")
	}
	if autohostRecreateAllowed("g-test") {
		t.Fatal("second recreate within the interval should be refused")
	}
	if !autohostRecreateAllowed("g-other") {
		t.Fatal("rate limit is per guild")
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

// 確認メッセージのボタンは autohost: で始まり (slash_commands.go の振り分け)、絵文字を必ず持つ
func TestAutohostConfirmResponse(t *testing.T) {
	r := autohostConfirmResponse("stop", "q", "ホスト終了")
	if r.Data.Flags != 1<<6 {
		t.Fatalf("confirm must be ephemeral")
	}
	row := r.Data.Components[0].(discordgo.ActionsRow)
	ids := []string{}
	for _, c := range row.Components {
		b := c.(discordgo.Button)
		if b.Emoji.Name == "" || !strings.HasPrefix(b.CustomID, autohostButtonPrefix) {
			t.Errorf("bad button %+v", b)
		}
		ids = append(ids, b.CustomID)
	}
	if ids[0] != "autohost:confirm:stop" || ids[1] != autohostBtnCancel {
		t.Fatalf("ids %v", ids)
	}
	if a := strings.TrimPrefix(strings.TrimPrefix(ids[0], autohostConfirmPrefix), autohostButtonPrefix); a != "stop" {
		t.Fatalf("action %q", a)
	}
}

package bot

import (
	"testing"

	"github.com/automuteus/automuteus/v8/pkg/amongus"
	"github.com/automuteus/automuteus/v8/pkg/settings"
)

func TestSpectatorsAsDead(t *testing.T) {
	dgs := GameState{
		UserData: UserDataSet{
			"1": {InGameName: "Shad"},
			"2": {InGameName: "あず ちん"},
			"3": {InGameName: amongus.UnlinkedPlayerName}, // 観戦者
		},
		GameData: amongus.NewGameData(),
	}
	dgs.GameData.PlayerData["shad"] = amongus.PlayerData{Name: "shad"}
	dgs.GameData.PlayerData["あずちん"] = amongus.PlayerData{Name: "あずちん"}

	sett := settings.MakeGuildSettings()
	if dgs.spectatorsAsDead(sett) {
		t.Fatal("mute-spectators off: spectators must not be treated as dead")
	}
	sett.SetMuteSpectator(true)
	if !dgs.AllPlayersLinked() || !dgs.spectatorsAsDead(sett) {
		t.Fatal("every player is linked: spectators should be treated as dead")
	}

	dgs.GameData.PlayerData["Rowa"] = amongus.PlayerData{Name: "Rowa"}
	if dgs.AllPlayersLinked() || dgs.spectatorsAsDead(sett) {
		t.Fatal("Rowa is not linked: an unlinked VC member may be Rowa")
	}
}

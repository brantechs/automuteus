package bot

import "testing"

func TestTohkSpeedAliveFinishStopsCounting(t *testing.T) {
	var s tohkSpeed
	s.add(tohkTaskGame{Role: "Crewmate", Team: "Crewmate", HasTasks: true, TasksTotal: 3, TasksDone: 3, Days: 6,
		Events: []tohkTaskEvt{{Day: 1, Done: 1, Alive: true}, {Day: 1, Done: 2, Alive: true}, {Day: 2, Done: 3, Alive: true}}})
	if s.AliveTasks != 3 || s.AliveTurns != 2 || s.HeavenTurns != 0 || s.EndDone != 3 || s.EndTotal != 3 {
		t.Fatalf("got %+v", s)
	}
}

func TestTohkSpeedDiedThenHeaven(t *testing.T) {
	var s tohkSpeed
	// 2 日目に死亡、天界で 3 日目と 4 日目に終える (最後のタスク時間は 4 日目: days=5)
	s.add(tohkTaskGame{Role: "Crewmate", Team: "Crewmate", HasTasks: true, TasksTotal: 4, TasksDone: 4, Dead: true, DeathDay: 2, Days: 5,
		Events: []tohkTaskEvt{{Day: 1, Done: 1, Alive: true}, {Day: 2, Done: 2, Alive: true}, {Day: 3, Done: 3}, {Day: 4, Done: 4}}})
	if s.AliveTasks != 2 || s.AliveTurns != 2 || s.HeavenTasks != 2 || s.HeavenTurns != 2 {
		t.Fatalf("got %+v", s)
	}
}

func TestTohkSpeedGuardianAngelSeparate(t *testing.T) {
	var s tohkSpeed
	s.add(tohkTaskGame{Role: "Crewmate", Team: "Crewmate", GhostRole: "GuardianAngel", HasTasks: true, TasksTotal: 5, TasksDone: 2,
		Dead: true, DeathDay: 1, Days: 4, Events: []tohkTaskEvt{{Day: 2, Done: 1}, {Day: 3, Done: 2}}})
	if s.GATasks != 2 || s.GATurns != 2 || s.HeavenTasks != 0 || s.AliveTurns != 1 {
		t.Fatalf("got %+v", s)
	}
}

func TestTohkSpeedExcluded(t *testing.T) {
	var s tohkSpeed
	s.add(tohkTaskGame{Role: "Sheriff", Team: "Crewmate", HasTasks: true, TasksTotal: 3})
	s.add(tohkTaskGame{Role: "MadSnitch", Team: "Madmate", HasTasks: true, TasksTotal: 3})
	s.add(tohkTaskGame{Role: "Crewmate", Team: "Crewmate", SubRoles: []string{"Madmate"}, HasTasks: true, TasksTotal: 3})
	s.add(tohkTaskGame{Role: "Crewmate", Team: "Crewmate", HasTasks: false})
	if s.Games != 0 {
		t.Fatalf("excluded roles were counted: %+v", s)
	}
	// 記録のない昔の試合は終了時の数だけ数える
	s.add(tohkTaskGame{Role: "Crewmate", Team: "Crewmate", HasTasks: true, TasksTotal: 4, TasksDone: 1, Days: 3})
	if s.Games != 1 || s.EventGames != 0 || s.EndDone != 1 || s.AliveTurns != 0 {
		t.Fatalf("got %+v", s)
	}
}

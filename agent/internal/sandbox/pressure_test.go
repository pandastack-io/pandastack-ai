// SPDX-License-Identifier: Apache-2.0
//
// Ladder decision-logic tests: level classification, coldness ordering,
// squeeze targets, and freeze eligibility. Pure functions — no cgroups.
package sandbox

import (
	"testing"
	"time"
)

func TestClassifyPressure(t *testing.T) {
	th := pressureThresholds{lowPct: 15, critPct: 8, psiSome: 10, psiFull: 5}
	const total = 32 * 1024 // 32 GiB host: 15% = 4915MB clamps to 4096, 8% = 2621MB clamps to 2048
	cases := []struct {
		availMB    int64
		some, full float64
		want       pressureLevel
	}{
		{16000, 0, 0, levelOK},
		{4095, 0, 0, levelElevated},  // below clamped low water (4096 MB)
		{4200, 11, 0, levelElevated}, // PSI some alone
		{2047, 0, 0, levelCritical},  // below clamped crit water (2048 MB)
		{16000, 0, 6, levelCritical}, // PSI full alone
		{2000, 99, 99, levelCritical},
	}
	for _, c := range cases {
		if got := classifyPressure(th, c.availMB, total, c.some, c.full); got != c.want {
			t.Errorf("classify(avail=%v some=%v full=%v)=%v want %v", c.availMB, c.some, c.full, got, c.want)
		}
	}
	// Small host: percentages bind (no clamp). 8 GiB → low 1228 MB, crit 655 MB.
	if got := classifyPressure(th, 1200, 8*1024, 0, 0); got != levelElevated {
		t.Errorf("small-host low water: got %v", got)
	}
}

func TestApplyHysteresis(t *testing.T) {
	streak := 0
	// Rising is immediate.
	if got := applyHysteresis(levelOK, levelCritical, &streak); got != levelCritical {
		t.Errorf("rise: got %v", got)
	}
	// Falling needs two consecutive ticks.
	if got := applyHysteresis(levelCritical, levelOK, &streak); got != levelCritical {
		t.Errorf("first fall tick must hold: got %v", got)
	}
	if got := applyHysteresis(levelCritical, levelOK, &streak); got != levelOK {
		t.Errorf("second fall tick must release: got %v", got)
	}
	// A rise resets the fall streak.
	streak = 1
	if got := applyHysteresis(levelElevated, levelElevated, &streak); got != levelElevated || streak != 0 {
		t.Errorf("steady/rise must reset streak: got %v streak=%d", got, streak)
	}
}

func TestColdnessLess(t *testing.T) {
	now := time.Now()
	hot := pressureVM{id: "hot", cpuRate: 2.0, lastAct: now}
	warm := pressureVM{id: "warm", cpuRate: 0.1, lastAct: now}
	coldNew := pressureVM{id: "cold-new", cpuRate: 0, lastAct: now}
	coldOld := pressureVM{id: "cold-old", cpuRate: 0, lastAct: now.Add(-time.Hour)}
	if !coldnessLess(warm, hot) || !coldnessLess(coldNew, warm) {
		t.Error("lower cpu rate must sort colder")
	}
	if !coldnessLess(coldOld, coldNew) {
		t.Error("equal cpu rate: older activity must sort colder")
	}
}

func TestSqueezeTarget(t *testing.T) {
	if got := squeezeTarget(1 << 30); got != (1<<30)*70/100 {
		t.Errorf("1GiB → %d, want 70%%", got)
	}
	if got := squeezeTarget(100 << 20); got != squeezeFloorBytes {
		t.Errorf("small VM must clamp to floor, got %d", got)
	}
}

func TestFreezeEligible(t *testing.T) {
	now := time.Now()
	old := now.Add(-10 * time.Minute)
	base := pressureVM{class: "sandbox", cpuRate: 0, lastAct: old}
	if !freezeEligible(base, old, now) {
		t.Error("cold idle plain sandbox must be eligible")
	}
	app := base
	app.class = "app"
	if freezeEligible(app, old, now) {
		t.Error("app class must never be ladder-frozen")
	}
	young := base
	if freezeEligible(young, now.Add(-30*time.Second), now) {
		t.Error("too-young VM must be ineligible")
	}
	active := base
	active.lastAct = now.Add(-10 * time.Second)
	if freezeEligible(active, old, now) {
		t.Error("recently-touched VM must be ineligible")
	}
	busy := base
	busy.cpuRate = 1.5
	if freezeEligible(busy, old, now) {
		t.Error("cpu-busy VM must be ineligible")
	}
	neverTouched := base
	neverTouched.lastAct = time.Time{}
	if !freezeEligible(neverTouched, old, now) {
		t.Error("zero lastAct (never touched) must be eligible")
	}
}

func TestCritWaterMB(t *testing.T) {
	th := pressureThresholds{critPct: 8}
	if got := critWaterMB(th, 32*1024); got != 2048 {
		t.Errorf("32GiB host must clamp to 2048, got %d", got)
	}
	if got := critWaterMB(th, 8*1024); got != 655 {
		t.Errorf("8GiB host: 8%% = 655, got %d", got)
	}
}

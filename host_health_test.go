package gotdx

import (
	"testing"
	"time"
)

func testHostView(addr string, score time.Duration, samples int, reachable bool) hostHealthView {
	return hostHealthView{
		Address:   addr,
		Reachable: reachable,
		Samples:   samples,
		Score:     score,
	}
}

func TestHostHealthViewComputesEWMAAndP95(t *testing.T) {
	h := &hostHealth{}
	for _, d := range []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 300 * time.Millisecond} {
		h.record(d)
	}
	v := h.view("a:1", true, 0)
	if v.Samples != 3 {
		t.Fatalf("samples=%d, want 3", v.Samples)
	}
	if v.P95 != 300*time.Millisecond {
		t.Fatalf("p95=%v, want 300ms", v.P95)
	}
	if v.EWMA <= 0 || v.Score <= 0 {
		t.Fatalf("invalid EWMA/score: %+v", v)
	}
}

func TestSelectHostTargetRequiresSamples(t *testing.T) {
	views := []hostHealthView{
		testHostView("a:1", 100*time.Millisecond, 5, true),
		testHostView("b:1", 70*time.Millisecond, 2, true),
	}
	state := &hostSwitchState{}
	if got, reason := selectHostTarget("a:1", views, state, time.Now()); got != "" || reason != "current-fastest" {
		t.Fatalf("target=%q reason=%q, want current-fastest/no switch", got, reason)
	}
}

// TestHostHealthDefaultSwitchTakesFiveCycles 验证默认 1 分钟一轮时：
// 前 3 轮收集样本，之后连续 3 轮确认，第 5 轮才真正切换（约 5 分钟）。
// 慢连接状态为 15 秒一轮，对应约 75 秒。
func TestHostHealthDefaultSwitchTakesFiveCycles(t *testing.T) {
	current := &hostHealth{}
	candidate := &hostHealth{}
	state := &hostSwitchState{}
	now := time.Now()
	var switched string
	for cycle := 1; cycle <= 5; cycle++ {
		current.record(100 * time.Millisecond)
		candidate.record(70 * time.Millisecond)
		views := []hostHealthView{
			current.view("a:1", true, 0),
			candidate.view("b:1", true, 0),
		}
		target, _ := selectHostTarget("a:1", views, state, now)
		if target != "" {
			switched = target
			if cycle != 5 {
				t.Fatalf("switched at cycle %d, want cycle 5", cycle)
			}
		}
	}
	if switched != "b:1" {
		t.Fatalf("did not switch after 5 cycles: %q", switched)
	}
}

func TestSelectHostTargetConfirmCycles(t *testing.T) {
	views := []hostHealthView{
		testHostView("a:1", 100*time.Millisecond, 5, true),
		testHostView("b:1", 70*time.Millisecond, 5, true),
	}
	state := &hostSwitchState{}
	now := time.Now()
	if got, _ := selectHostTarget("a:1", views, state, now); got != "" {
		t.Fatalf("cycle 1 switched to %q", got)
	}
	if got, _ := selectHostTarget("a:1", views, state, now); got != "" {
		t.Fatalf("cycle 2 switched to %q", got)
	}
	if got, reason := selectHostTarget("a:1", views, state, now); got != "b:1" || reason != "confirmed" {
		t.Fatalf("cycle 3 target=%q reason=%q, want b:1/confirmed", got, reason)
	}
}

func TestSelectHostTargetHysteresis(t *testing.T) {
	// b 只快 10%，没有达到 20% 滞回阈值。
	views := []hostHealthView{
		testHostView("a:1", 100*time.Millisecond, 5, true),
		testHostView("b:1", 90*time.Millisecond, 5, true),
	}
	state := &hostSwitchState{}
	for i := 0; i < 5; i++ {
		if got, _ := selectHostTarget("a:1", views, state, time.Now()); got != "" {
			t.Fatalf("hysteresis switched to %q", got)
		}
	}
	// b 快 30%，满足正常切换阈值；仍需 3 轮确认。
	views[1].Score = 70 * time.Millisecond
	state = &hostSwitchState{}
	for i := 0; i < 2; i++ {
		if got, _ := selectHostTarget("a:1", views, state, time.Now()); got != "" {
			t.Fatalf("switched before confirmation at cycle %d", i+1)
		}
	}
	if got, _ := selectHostTarget("a:1", views, state, time.Now()); got != "b:1" {
		t.Fatalf("did not switch after confirmation: %q", got)
	}
}

func TestSelectHostTargetCooldown(t *testing.T) {
	views := []hostHealthView{
		testHostView("a:1", 100*time.Millisecond, 5, true),
		testHostView("b:1", 50*time.Millisecond, 5, true),
	}
	now := time.Now()
	state := &hostSwitchState{lastSwitchAt: now.Add(-time.Minute)}
	for i := 0; i < 5; i++ {
		if got, _ := selectHostTarget("a:1", views, state, now); got != "" {
			t.Fatalf("cooldown switched to %q", got)
		}
	}
	state.lastSwitchAt = now.Add(-hostSwitchCooldown - time.Second)
	for i := 0; i < 2; i++ {
		if got, _ := selectHostTarget("a:1", views, state, now); got != "" {
			t.Fatalf("switched before confirmation after cooldown at cycle %d", i+1)
		}
	}
	if got, _ := selectHostTarget("a:1", views, state, now); got != "b:1" {
		t.Fatalf("did not switch after cooldown: %q", got)
	}
}

func TestWithHostSwitchObserverOption(t *testing.T) {
	called := false
	c := newClientWithOptions(applyOptions(WithHostSwitchObserver(func(HostSwitchEvent) { called = true })), clientModeMacMain)
	if c.opt.OnHostSwitch == nil {
		t.Fatal("observer not set")
	}
	c.opt.OnHostSwitch(HostSwitchEvent{From: "a", To: "b"})
	if !called {
		t.Fatal("observer was not called")
	}
}

func TestSelectHostTargetEmergencyOnCurrentFailures(t *testing.T) {
	views := []hostHealthView{
		testHostView("a:1", 100*time.Millisecond, 5, true),
		testHostView("b:1", 70*time.Millisecond, 5, true),
	}
	views[0].Failures = hostSwitchEmergencyFailures
	// 冷却期内也要允许故障切换，不能把用户继续钉在坏 host 上。
	state := &hostSwitchState{lastSwitchAt: time.Now()}
	if got, reason := selectHostTarget("a:1", views, state, time.Now()); got != "b:1" || reason != "current-failing" {
		t.Fatalf("target=%q reason=%q, want b:1/current-failing", got, reason)
	}
}

func TestSelectHostTargetCurrentUnavailable(t *testing.T) {
	views := []hostHealthView{
		testHostView("a:1", 100*time.Millisecond, 0, false),
		testHostView("b:1", 80*time.Millisecond, 5, true),
	}
	if got, reason := selectHostTarget("a:1", views, &hostSwitchState{}, time.Now()); got != "b:1" || reason != "current-unavailable" {
		t.Fatalf("target=%q reason=%q, want b:1/current-unavailable", got, reason)
	}
}

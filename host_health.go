package gotdx

import (
	"math"
	"sort"
	"time"
)

// HostHealth 是一组可测试的 MAC 主机业务健康参数。
//
// 采样周期由 rebalance tick 决定：健康状态默认 1 分钟一轮，慢连接状态 15 秒一轮。
// 每个候选在每轮探活中执行一次真实 MACSymbolBars，因此：
//   - 默认情况下前 3 轮积累最少 3 个样本，之后再连续 3 轮确认，约 5 分钟完成切换；
//   - 慢连接触发 15 秒重平衡时，最快约 75 秒完成；
//   - 切换后有 10 分钟冷却，避免来回抖动。
const (
	hostHealthSampleLimit    = 32
	hostHealthMinSamples     = 3
	hostHealthEWMAAlpha      = 0.25
	hostSwitchConfirmCycles  = 3
	hostSwitchHysteresis     = 0.80 // 候选必须比当前快至少 20%，正常切换
	hostSwitchSlowHysteresis = 0.90 // 当前已出现失败/慢样本时，候选快 10% 即可
	hostSwitchCooldown       = 10 * time.Minute
	hostFailurePenalty       = 500 * time.Millisecond
)

// HostSwitchEvent 描述一次已经完成的 MAC 主机切换。
type HostSwitchEvent struct {
	From      string
	To        string
	Reason    string
	FromScore time.Duration
	ToScore   time.Duration
	At        time.Time
}

// hostHealth 是单个地址的 K 线业务延迟统计。
type hostHealth struct {
	samples       []time.Duration
	ewma          time.Duration
	failureStreak int
}

// hostHealthView 是一次排序使用的不可变快照。
type hostHealthView struct {
	Address   string
	Reachable bool
	Samples   int
	EWMA      time.Duration
	P95       time.Duration
	Failures  int
	Score     time.Duration
}

// hostSwitchState 保存连续确认与冷却状态。
type hostSwitchState struct {
	candidate    string
	streak       int
	lastSwitchAt time.Time
}

func (h *hostHealth) record(d time.Duration) {
	if h == nil || d <= 0 {
		return
	}
	if len(h.samples) >= hostHealthSampleLimit {
		copy(h.samples, h.samples[1:])
		h.samples[len(h.samples)-1] = d
	} else {
		h.samples = append(h.samples, d)
	}
	if h.ewma <= 0 {
		h.ewma = d
	} else {
		h.ewma = time.Duration(float64(h.ewma)*(1-hostHealthEWMAAlpha) + float64(d)*hostHealthEWMAAlpha)
	}
	h.failureStreak = 0
}

func (h *hostHealth) recordFailure() {
	if h == nil {
		return
	}
	h.failureStreak++
}

func (h *hostHealth) view(address string, reachable bool, fallback time.Duration) hostHealthView {
	v := hostHealthView{
		Address:   address,
		Reachable: reachable,
		Samples:   len(h.samples),
		EWMA:      h.ewma,
		Failures:  h.failureStreak,
	}
	if len(h.samples) > 0 {
		sorted := append([]time.Duration(nil), h.samples...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		idx := int(math.Ceil(0.95*float64(len(sorted)))) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(sorted) {
			idx = len(sorted) - 1
		}
		v.P95 = sorted[idx]
	}
	// 样本不足时用本轮探活值兜底，但不允许该候选参与切换。
	if v.Samples < hostHealthMinSamples {
		v.Score = fallback
	} else {
		// EWMA 反映近期趋势，p95 覆盖尾部抖动；两者平均避免单个尖峰主导。
		v.Score = (v.EWMA*3 + v.P95) / 4
		if v.Failures > 0 {
			v.Score += time.Duration(v.Failures) * hostFailurePenalty
		}
	}
	return v
}

// selectHostTarget 是纯函数，方便单元测试确认：
// 当前 host 可用时，候选必须样本充足、达到滞回阈值并连续确认；
// 当前 host 不可达时直接选择最优可达候选；切换后冷却期内不重复切。
func selectHostTarget(current string, views []hostHealthView, state *hostSwitchState, now time.Time) (string, string) {
	if state == nil {
		return "", "nil-state"
	}
	var best *hostHealthView
	for i := range views {
		v := &views[i]
		if !v.Reachable || v.Samples < hostHealthMinSamples || v.Score <= 0 {
			continue
		}
		if best == nil || v.Score < best.Score || (v.Score == best.Score && v.Address < best.Address) {
			best = v
		}
	}
	if best == nil {
		state.candidate = ""
		state.streak = 0
		return "", "no-qualified-candidate"
	}
	var currentView *hostHealthView
	for i := range views {
		if views[i].Address == current {
			currentView = &views[i]
			break
		}
	}
	if current == "" || currentView == nil || !currentView.Reachable {
		state.candidate = ""
		state.streak = 0
		return best.Address, "current-unavailable"
	}
	if best.Address == current {
		state.candidate = ""
		state.streak = 0
		return "", "current-fastest"
	}
	if !state.lastSwitchAt.IsZero() && now.Sub(state.lastSwitchAt) < hostSwitchCooldown {
		state.candidate = ""
		state.streak = 0
		return "", "cooldown"
	}
	threshold := hostSwitchHysteresis
	if currentView.Failures > 0 {
		threshold = hostSwitchSlowHysteresis
	}
	if float64(best.Score) >= float64(currentView.Score)*threshold {
		state.candidate = ""
		state.streak = 0
		return "", "hysteresis"
	}
	if state.candidate != best.Address {
		state.candidate = best.Address
		state.streak = 1
	} else {
		state.streak++
	}
	if state.streak < hostSwitchConfirmCycles {
		return "", "confirming"
	}
	return best.Address, "confirmed"
}

func (client *Client) recordHostSampleLocked(addr string, elapsed time.Duration) {
	if client == nil || addr == "" || elapsed <= 0 {
		return
	}
	if client.hostHealth == nil {
		client.hostHealth = make(map[string]*hostHealth)
	}
	h := client.hostHealth[addr]
	if h == nil {
		h = &hostHealth{}
		client.hostHealth[addr] = h
	}
	h.record(elapsed)
}

func (client *Client) recordHostFailureLocked(addr string) {
	if client == nil || addr == "" {
		return
	}
	if client.hostHealth == nil {
		client.hostHealth = make(map[string]*hostHealth)
	}
	h := client.hostHealth[addr]
	if h == nil {
		h = &hostHealth{}
		client.hostHealth[addr] = h
	}
	h.recordFailure()
}

// hostHealthViewLocked 生成单个地址的健康视图；fallback 是本次 K 线探活的实测值。
func (client *Client) hostHealthViewLocked(addr string, reachable bool, fallback time.Duration) hostHealthView {
	var h *hostHealth
	if client != nil && client.hostHealth != nil {
		h = client.hostHealth[addr]
	}
	if h == nil {
		return hostHealthView{Address: addr, Reachable: reachable, Score: fallback}
	}
	return h.view(addr, reachable, fallback)
}

// HostHealthSnapshot 返回当前 MAC 主机健康快照，供诊断/测试使用。
func (client *Client) HostHealthSnapshot() []hostHealthView {
	if client == nil {
		return nil
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	result := make([]hostHealthView, 0, len(client.hostHealth))
	for addr, h := range client.hostHealth {
		result = append(result, h.view(addr, true, 0))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Address < result[j].Address })
	return result
}

package behavior

import (
	"math"
	"testing"
)

// 本文件的边界校核样例直接对应冻结文档（v4.0）附录 A 的表格——文档表格即测试
// 规格：任何公式变更（modelVersion+1）必须先改文档再改此处。

func TestScoreAQuantileChannel(t *testing.T) {
	const p50, p90 = 10.0, 20.0
	cases := []struct {
		x    float64
		want float64 // 100·log2(r)/3, r = x/20
	}{
		{20, 0},                      // r=1 → 0
		{40, 100 * math.Log2(2) / 3}, // r=2 → ~33.3
		{80, 100 * math.Log2(4) / 3}, // r=4 → ~66.7
		{160, 100},                   // r=8 → 满分
		{10, 0},                      // 低于参考 → 0
	}
	for _, c := range cases {
		if got := ScoreA(c.x, p50, p90, 0, 0, true, false); math.Abs(got-c.want) > 0.5 {
			t.Errorf("ScoreA(%v) = %v, want ~%v", c.x, got, c.want)
		}
	}
	// 低流量 floor：P50=2、P90=3，floorA=max(2,5)=5 → ref=max(3,2,5)=5。
	if got := ScoreA(40, 2, 3, 0, 0, true, false); math.Abs(got-100*math.Log2(8)/3) > 0.5 {
		t.Errorf("low-traffic floor: ScoreA = %v, want ~100 (r=8)", got)
	}
}

func TestScoreAEWMAStricter(t *testing.T) {
	// 分位通道宽松（P90=50）但 EWMA 通道紧（ewma=10、var=0 → refE=10）：取严后满分。
	if got := ScoreA(80, 10, 50, 10, 0, true, true); got != 100 {
		t.Errorf("EWMA-stricter case: ScoreA = %v, want 100", got)
	}
	// 反之亦然：EWMA 宽松、分位紧 → 取分位。
	if got := ScoreA(80, 10, 20, 1000, 0, true, true); math.Abs(got-100*math.Log2(4)/3) > 0.5 {
		t.Errorf("quantile-stricter case: ScoreA = %v, want ~66.7", got)
	}
}

func TestScoreBFloorAndNormal(t *testing.T) {
	// 噪声地板：mad 极小时被抬到 0.072（log₂）——量化误差同量级。
	// med=1000ms（log2≈9.966），yNow=2000ms（log2=10），z=(10-9.966)/0.072≈0.47 → s≈4.7。
	if got := ScoreB(10, 9.965784284662087, 0.0001); got > 8 {
		t.Errorf("ScoreB with tiny mad = %v, floor must keep it low", got)
	}
	// 正常路径：mad=0.5，yNow 偏离 med 5 个 mad → s = 100·5/10 = 50。
	if got := ScoreB(9.965784284662087+5*0.5, 9.965784284662087, 0.5); math.Abs(got-50) > 1 {
		t.Errorf("ScoreB normal = %v, want ~50", got)
	}
	// 变快不评分。
	if got := ScoreB(5, 9.97, 0.5); got != 0 {
		t.Errorf("ScoreB faster = %v, want 0", got)
	}
}

// TestScoreCBoundaryCases 对应冻结文档附录 A 的 C 类边界校核表。
func TestScoreCBoundaryCases(t *testing.T) {
	// 样例 1：全零基线·单次护栏触发（minNum=1）→ z≈12.6 → s=100。
	s, ok := ScoreC(0, 5000, 1, 30, 1)
	if !ok || s != 100 {
		t.Errorf("zero-base guardrail single hit: s=%v ok=%v, want s=100 ok=true", s, ok)
	}
	// 样例 2：全零基线·单次错误（minNum=2）→ no-score。
	s, ok = ScoreC(0, 5000, 1, 30, 2)
	if ok || s != 0 {
		t.Errorf("zero-base single error: s=%v ok=%v, want no-score", s, ok)
	}
	// 样例 3：极低基线 0.1%→8%（0.5% 起点）→ z≈15.5 → s=100。
	s, ok = ScoreC(5, 5000, 8, 100, 2)
	if !ok || s != 100 {
		t.Errorf("very-low-base: s=%v ok=%v, want s=100", s, ok)
	}
	// 样例 4：小分母 2%→13.3% → z≈3.76 → s≈47。
	s, ok = ScoreC(10, 500, 4, 30, 2)
	if !ok || s < 40 || s > 55 {
		t.Errorf("small-denominator: s=%v ok=%v, want s in [40,55]", s, ok)
	}
	// 分母门槛：比较窗 <30 → no-score（§5.1）。
	if _, ok := ScoreC(10, 5000, 5, 20, 1); ok {
		t.Error("n_now < 30 must be no-score")
	}
	// 基线分母门槛 <500 → no-score。
	if _, ok := ScoreC(10, 400, 5, 30, 1); ok {
		t.Error("n_base < 500 must be no-score")
	}
	// 比率下降 → 0 分但可评分。
	s, ok = ScoreC(50, 5000, 1, 100, 1)
	if !ok || s != 0 {
		t.Errorf("rate drop: s=%v ok=%v, want 0/scoreable", s, ok)
	}
}

func TestScoreNOrgPriorSuppression(t *testing.T) {
	if got := ScoreN(0.5, false); got != 50 {
		t.Errorf("ScoreN(0.5, no prior) = %v, want 50", got)
	}
	if got := ScoreN(0.5, true); got != 25 {
		t.Errorf("ScoreN(0.5, org prior) = %v, want 25 (halved)", got)
	}
}

func TestCompose(t *testing.T) {
	dims := []WeightedDimension{
		{Metric: "request_rate", Score: 80, Weight: 0.30},
		{Metric: "tokens:in", Score: 50, Weight: 0.15},
		{Metric: "cost:CNY", Score: 50, Weight: 0.15},
		{Metric: "latency", Score: 30, Weight: 0.10},
		{Metric: "err:gw", Score: 0, Weight: 0.10},
		{Metric: "gr:trigger", Score: 0, Weight: 0.10},
		{Metric: "model:*", Score: 0, Weight: 0.05},
		{Metric: "dormant", Score: 0, Weight: 0.05},
	}
	// 分母只含有信号维度（s'>0）：Σw=0.70、Σws=42 → 60，命中 3 个（80/50/50）→ ×1.2 = 72。
	want := (0.30*80 + 0.15*50 + 0.15*50 + 0.10*30) / 0.70 * 1.2
	if got := Compose(dims); math.Abs(got-want) > 0.5 {
		t.Errorf("Compose = %v, want ~%v", got, want)
	}
	// 少于 3 个命中维度 → 无加成：Σws=34.35/0.70 ≈ 49.1（39 分不入命中）。
	dims[1].Score, dims[2].Score = 39, 10
	want = (0.30*80 + 0.15*39 + 0.15*10 + 0.10*30) / 0.70
	if got := Compose(dims); math.Abs(got-want) > 0.5 {
		t.Errorf("Compose no-bonus = %v, want ~%v", got, want)
	}
	// 单维满分不被零分/量化噪声维度稀释（G0 剧本：速率 10×、其余正常 → critical；
	// s=2.4 的 latency 尾数曾把 S 稀释到 ~71）。
	single := []WeightedDimension{
		{Metric: "request_rate", Score: 100, Weight: 0.30},
		{Metric: "latency", Score: 2.4, Weight: 0.10},
		{Metric: "cost", Score: 0, Weight: 0.15},
	}
	if got := Compose(single); got != 100 {
		t.Errorf("Compose single-dim spike = %v, want 100 (no dilution by noise dims)", got)
	}
	// 小样本降权：全部 HalfSample → 权重均 ×0.5，等比不变。
	dims2 := []WeightedDimension{{Metric: "a", Score: 100, Weight: 1, HalfSample: true}}
	if got := Compose(dims2); got != 100 {
		t.Errorf("Compose single half-sample = %v, want 100", got)
	}
	// 全零 → 0（无事件）。
	if got := Compose([]WeightedDimension{{Metric: "x", Score: 0, Weight: 1}}); got != 0 {
		t.Errorf("Compose all-zero = %v, want 0", got)
	}
}

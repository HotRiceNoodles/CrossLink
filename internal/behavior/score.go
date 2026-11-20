package behavior

import "math"

// 打分公式（规划冻结 v4.0 附录 A）。纯函数、无状态；M1 交付"Community 接口 +
// 基础统计实现"（D4），M2 台账接线时消费。公式/权重/门槛任何变更 = modelVersion+1。
//
// 约定：s ∈ [0,100]；分数只升不降地映射正向异常（骤降不评分，§5.4）。

// MinObsPerBucket 是 A 类 hour-of-day 桶的打分门槛（该桶 ≥14 个活跃观测才计分，
// §5.1）。不满足=no-score（门槛判断在调用方，快照的 obs_count 供查）。
const MinObsPerBucket = 14

// ScoreA 是 A 类（计数/求和）得分：分位超越倍数的对数映射（D12：参考 P90）。
//
//	r = x / max(P90, P50, floorA)，floorA = max(P50, 5)   # 低流量防小数放大
//	s_q = clamp(100·log2(r)/3, 0, 100)                     # 2×=33 分、4×=67、8×=100
//
// EWMA 通道同式（参考 max(ewma+3σ, floorA)），双通道取严 s = max(s_q, s_ewma)。
// hasQuantile=false（基线桶不存在）返回 0——该维度 no-score 由调用方根据 obs_count 决定。
func ScoreA(xNow, p50, p90, ewma, ewmaVar float64, hasQuantile, hasEWMA bool) float64 {
	s := 0.0
	if hasQuantile {
		floorA := math.Max(p50, 5)
		ref := math.Max(math.Max(p90, p50), floorA)
		if ref <= 0 {
			ref = 5
		}
		s = ratioScore(xNow, ref)
	}
	if hasEWMA {
		floorA := math.Max(p50, 5)
		refE := ewma + 3*math.Sqrt(math.Max(ewmaVar, 0))
		refE = math.Max(refE, floorA)
		if refE <= 0 {
			refE = 5
		}
		if se := ratioScore(xNow, refE); se > s {
			s = se
		}
	}
	return s
}

// ratioScore 是超越倍数的对数映射：r≤1 → 0 分；8 倍 → 100 分。
func ratioScore(x, ref float64) float64 {
	if x <= ref || ref <= 0 {
		return 0
	}
	r := x / ref
	s := 100 * math.Log2(r) / 3
	return clamp(s, 0, 100)
}

// ScoreB 是 B 类（单请求分布）得分：log₂ 尺度稳健 z（MAD，附录 A）。
//
//	z = (y_now − m) / mad，mad 有噪声地板（量化误差同量级）
//	s = clamp(100·z/10, 0, 100)      # z=10 满分；只取正向（变慢）
//
// m/mad/yNow 均为 log₂ 尺度。地板 0.072 log₂ 单位 ≈ 0.05 ln 单位（冻结文档以 ln
// 表述，此处于 log₂ 尺度等价实现）。
func ScoreB(yNowLog2, medLog2, madLog2 float64) float64 {
	const madFloorLog2 = 0.072
	if madLog2 < madFloorLog2 {
		madLog2 = madFloorLog2
	}
	z := (yNowLog2 - medLog2) / madLog2
	return clamp(100*z/10, 0, 100)
}

// ScoreC 是 C 类（比率）得分：合并方差两比例 z（D18）。
//
//	p_pool = (x_base+x_now)/N，N := n_base+n_now   # N 唯一定义
//	var = p_pool(1−p_pool)·(1/n_now+1/n_base)      # x_now 入池 → 零方差仅在无信号时出现
//	s = clamp(100·z/8, 0, 100)                     # z=8 满分
//
// eligible=false 表示 no-score：最小分子门槛未达（error_rate 需 x_now≥2、
// guardrail_trigger 需 x_now≥1）或分母门槛未达（§5.1：比较窗分母 ≥30、基线分母 ≥500）。
func ScoreC(xBase, nBase, xNow, nNow float64, minNum int64) (s float64, eligible bool) {
	if xNow < float64(minNum) || nNow < 30 || nBase < 500 || nNow <= 0 || nBase <= 0 {
		return 0, false
	}
	pBase := xBase / nBase
	pNow := xNow / nNow
	if pNow <= pBase {
		return 0, true // 方向为降/平——记为可评分但 0 分（骤降不评分）
	}
	n := nBase + nNow
	pPool := (xBase + xNow) / n
	if pPool <= 0 || pPool >= 1 {
		return 0, true
	}
	v := pPool * (1 - pPool) * (1/nNow + 1/nBase)
	z := (pNow - pBase) / math.Sqrt(math.Max(v, 1e-12))
	return clamp(100*z/8, 0, 100), true
}

// ScoreN 是 N 类（新颖度）得分（§5.10）：s = 100·share，org 先验存在时减半。
func ScoreN(share float64, orgPrior bool) float64 {
	s := clamp(100*share, 0, 100)
	if orgPrior {
		s /= 2
	}
	return s
}

// WeightedDimension 是合成公式的输入（附录 A 合成块）。
type WeightedDimension struct {
	Metric string
	Score  float64 // 已施加容忍度乘子的 s'ᵢ（D14：s'ᵢ = sᵢ/tᵢⱼ）
	Weight float64
	// HalfSample 置 true 时权重 ×0.5（obs < 2×门槛的小样本降权）。
	HalfSample bool
}

// signalFloor 是合成参与线：s'ᵢ < 10 视为量化噪声（直方图量化/MAD 地板产生的
// s≈2 级尾数），不入合成——否则一个 s=2.4 的 latency 尾数会把单因剧本的 S 从
// 100 稀释到 ~71（G0 剧本集实测）。常量冻结于 modelVersion 1。
const signalFloor = 10.0

// Compose 合成异常分：
//
//	w'ᵢ = wᵢ·(小样本? 0.5 : 1)
//	S = min(100, (Σ_{s'ᵢ≥signalFloor} w'ᵢ·sᵢ / Σ_{s'ᵢ≥signalFloor} w'ᵢ) · (≥3 个维度 s'ᵢ≥40 ? 1.2 : 1))
//
// 分母只含有信号维度：零分/噪声维度若计入，"速率 10×、其余正常"的 G0 必检
// 剧本会被稀释到永不触发——G0 门槛（单剧本检出 ≥90%）即此消歧依据
// （冻结 v4.0 附录 A 的 Σw'ᵢ 口径补注，2026-11-20；signalFloor 同日补注）。
// 命中门槛 40 为固定常量（D14）。
func Compose(dims []WeightedDimension) float64 {
	var sumW, sumWS float64
	hits := 0
	for _, d := range dims {
		if d.Score < signalFloor {
			continue // 零分与量化噪声维度不参与合成
		}
		w := d.Weight
		if d.HalfSample {
			w *= 0.5
		}
		sumW += w
		sumWS += w * clamp(d.Score, 0, 100)
		if d.Score >= 40 {
			hits++
		}
	}
	if sumW <= 0 {
		return 0
	}
	s := sumWS / sumW
	if hits >= 3 {
		s *= 1.2
	}
	return clamp(s, 0, 100)
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

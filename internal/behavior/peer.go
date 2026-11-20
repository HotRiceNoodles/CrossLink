package behavior

import (
	"math"
	"sort"
)

// P 通道（规划冻结 v4.0 D17：群体对比，防 grooming 的第二道闸）。
//
// key 的 A 类 request_rate 对照 org 内活跃 key 分布（log₂ 尺度跨 key
// median/MAD）。启用条件：org 内当小时活跃 key ≥5。输出 peer_z 与 peer_flag；
// dominant_key（占 org 当小时请求 >50%）对该 key no-score——它自己主导分布，
// P 通道保护的是非主导 key（§5.6）。
//
// 独立开案语义（M4 补注，2026-11-20）：grooming 剧本下自身基线已适应、自身维度
// 全灭——P 通道若只能注记既有事件将永远无事件（G0 门槛 4 的通路缺口）。故 P 通道
// 作为可独立存在的维度（class P，权重 0.10）：s = clamp(100·log₂(ratio)/2.5)，
// ratio = 自身值/org 中位。比自身通道的 /3 略敏感——第二道闸的本义。

const (
	peerMinKeys    = 5
	peerMADFloor   = 0.072 // log₂ 尺度噪声地板（与 ScoreB 同款）
	peerDominantFr = 0.5   // 占 org 当小时请求 >50% → dominant，P 通道 no-score
)

type peerStats struct {
	nKeys   int
	median  float64 // log₂ 中位
	mad     float64 // log₂ MAD
	dominan bool    // 该 key 是 dominant
	ratio   float64 // 自身/org 中位（线性尺度）
	z       float64 // log₂ 稳健 z
}

// computePeerChannel 由当小时的 key 级 request_rate rollup + key→org 映射计算
// 每 org 的跨 key 分布（纯内存，零额外查询）。
func computePeerChannel(hourRows []hourRollupRow, orgOf map[int64]int64) map[int64]*peerStats {
	type orgAcc struct {
		vals []float64 // log₂ 值（活跃 key）
	}
	byOrg := map[int64]*orgAcc{}
	keyVal := map[int64]float64{}
	orgTotal := map[int64]float64{}
	for _, r := range hourRows {
		if r.EntityType != EntityTypeKey || r.Metric != MetricRequestRate || r.ValueNum <= 0 {
			continue
		}
		keyVal[r.EntityID] = r.ValueNum
		org, ok := orgOf[r.EntityID]
		if !ok {
			continue
		}
		orgTotal[org] += r.ValueNum
		acc := byOrg[org]
		if acc == nil {
			acc = &orgAcc{}
			byOrg[org] = acc
		}
		acc.vals = append(acc.vals, math.Log2(r.ValueNum))
	}
	out := map[int64]*peerStats{}
	for org, acc := range byOrg {
		if len(acc.vals) < peerMinKeys {
			continue
		}
		med := medianF(acc.vals)
		mad := 1.4826 * medianAbsDev(acc.vals, med)
		if mad < peerMADFloor {
			mad = peerMADFloor
		}
		medLinear := math.Pow(2, med)
		if medLinear <= 0 {
			continue
		}
		for keyID, v := range keyVal {
			if o, ok := orgOf[keyID]; !ok || o != org {
				continue
			}
			dominant := v > peerDominantFr*orgTotal[org]
			lv := math.Log2(v)
			out[keyID] = &peerStats{
				nKeys:   len(acc.vals),
				median:  med,
				mad:     mad,
				dominan: dominant,
				ratio:   v / medLinear,
				z:       (lv - med) / mad,
			}
		}
	}
	return out
}

// ScorePeer 是 P 通道维度得分：ratio 线性超越倍数的对数映射（/2.5——比自身
// 通道 /3 略敏感，第二道闸本义）。dominant 或样本不足返回 0（no-score）。
func ScorePeer(p *peerStats) (float64, bool) {
	if p == nil || p.dominan || p.nKeys < peerMinKeys || p.ratio <= 1 {
		return 0, false
	}
	return clamp(100*math.Log2(p.ratio)/2.5, 0, 100), true
}

func medianF(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

func medianAbsDev(v []float64, med float64) float64 {
	devs := make([]float64, len(v))
	for i, x := range v {
		devs[i] = math.Abs(x - med)
	}
	return medianF(devs)
}

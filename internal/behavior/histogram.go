package behavior

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
)

// LogHistogram 是 B 类指标的 log₂ 直方图（规划冻结 v4.0 D11）：
//
//	bin = floor(log₂(x) · 8)   → 桶宽 2^(1/8)，相对误差 ≤ ~9%
//
// 只存非空桶（稀疏 [[bin,count],...]），合并 = 逐桶相加，分位数 = 定位累计
// 跨越点 + 桶内对数线性插值。量化误差 ≤ 半桶宽 ≈ 0.043 ln 单位，与打分公式的
// MAD 下限 0.05（ln 尺度）同量级——两者同为噪声地板（附录 A）。
type LogHistogram struct {
	bins  map[int32]uint64
	total uint64
}

// NewLogHistogram 创建空直方图。
func NewLogHistogram() *LogHistogram {
	return &LogHistogram{bins: make(map[int32]uint64)}
}

// BinOf 返回 x 所属的 log₂ 桶号：bin = floor(log₂(x) · 8)。x ≤ 0 无定义
// （调用方保证只 Add 正值）。
func BinOf(x float64) int32 {
	return int32(math.Floor(math.Log2(x) * 8))
}

// binLowerBound 返回桶 b 的下界 2^(b/8)。
func binLowerBound(b int32) float64 {
	return math.Pow(2, float64(b)/8)
}

// Add 记录一个观测值；x ≤ 0 跳过（延迟 0/NULL 不入池，§5.5 缺失约定）。
func (h *LogHistogram) Add(x float64) {
	if x <= 0 {
		return
	}
	h.bins[BinOf(x)]++
	h.total++
}

// AddBin 直接累加一个桶（反序列化/合并用）。
func (h *LogHistogram) AddBin(bin int32, count uint64) {
	if count == 0 {
		return
	}
	h.bins[bin] += count
	h.total += count
}

// Merge 就地合并另一个直方图（逐桶相加）。
func (h *LogHistogram) Merge(o *LogHistogram) {
	for b, c := range o.bins {
		h.AddBin(b, c)
	}
}

// Count 返回累计观测数。
func (h *LogHistogram) Count() uint64 { return h.total }

// Quantile 返回 q ∈ [0,1] 分位数（对数线性插值）。空直方图返回 0。
//
// 桶内插值在对数空间线性：bin b 覆盖 [2^(b/8), 2^((b+1)/8))，位置 pos 的取值
// 为 lower·(upper/lower)^pos。
func (h *LogHistogram) Quantile(q float64) float64 {
	if h.total == 0 || q <= 0 {
		return 0
	}
	if q >= 1 {
		q = 1
	}
	target := uint64(0)
	if q < 1 {
		// 第 ceil(q·N) 个观测（最近秩）；q=1 时取最后越过点。
		target = uint64(q*float64(h.total) + 0.999999)
		if target < 1 {
			target = 1
		}
		if target > h.total {
			target = h.total
		}
	}
	bins := h.sortedBins()
	var cum uint64
	for _, b := range bins {
		c := h.bins[b]
		if q >= 1 || cum+c >= target {
			lower := binLowerBound(b)
			// 桶内位置（对数线性）：bin b 覆盖 [lower, lower·2^(1/8))。
			var pos float64
			if c > 0 {
				if q >= 1 {
					pos = 1
				} else {
					pos = (float64(target) - float64(cum) - 0.5) / float64(c)
					if pos < 0 {
						pos = 0
					}
					if pos > 1 {
						pos = 1
					}
				}
			}
			return lower * math.Pow(2, pos/8) // lower·(upper/lower)^pos, upper/lower = 2^(1/8)
		}
		cum += c
	}
	return binLowerBound(bins[len(bins)-1])
}

// Median 返回中位数。
func (h *LogHistogram) Median() float64 { return h.Quantile(0.5) }

// QuartileSpread 返回 (q75 − q25)/2，MAD 的分布版代理：附录 A 的 mad 以
// quantile(H,0.75)−quantile(H,0.25) 的一半计算（冻结文档 B 类公式）。
func (h *LogHistogram) QuartileSpread() float64 {
	return (h.Quantile(0.75) - h.Quantile(0.25)) / 2
}

func (h *LogHistogram) sortedBins() []int32 {
	bs := make([]int32, 0, len(h.bins))
	for b := range h.bins {
		bs = append(bs, b)
	}
	sort.Slice(bs, func(i, j int) bool { return bs[i] < bs[j] })
	return bs
}

// MarshalJSON 序列化为稀疏 [[bin,count],...]（按 bin 升序）。
func (h *LogHistogram) MarshalJSON() ([]byte, error) {
	bins := h.sortedBins()
	pairs := make([][2]uint64, 0, len(bins))
	for _, b := range bins {
		pairs = append(pairs, [2]uint64{uint64(int64(b)), h.bins[b]})
	}
	return json.Marshal(pairs)
}

// UnmarshalJSON 解析稀疏 [[bin,count],...]（bin 为有符号数，以 uint64 传输，
// 负 bin 经 int64 还原）。
func (h *LogHistogram) UnmarshalJSON(data []byte) error {
	var pairs [][2]uint64
	if err := json.Unmarshal(data, &pairs); err != nil {
		return err
	}
	h.bins = make(map[int32]uint64, len(pairs))
	h.total = 0
	for _, p := range pairs {
		h.AddBin(int32(int64(p[0])), p[1])
	}
	return nil
}

// String 实现 fmt.Stringer（调试用）。
func (h *LogHistogram) String() string {
	b, err := h.MarshalJSON()
	if err != nil {
		return fmt.Sprintf("LogHistogram(%d obs, marshal error)", h.total)
	}
	return fmt.Sprintf("LogHistogram(%d obs, %s)", h.total, string(b))
}

// ParseLogHistogram 从存储 JSON（rollups.value_hist）解析。
func ParseLogHistogram(s string) (*LogHistogram, error) {
	h := NewLogHistogram()
	if s == "" {
		return h, nil
	}
	if err := h.UnmarshalJSON([]byte(s)); err != nil {
		return nil, fmt.Errorf("parse log histogram: %w", err)
	}
	return h, nil
}

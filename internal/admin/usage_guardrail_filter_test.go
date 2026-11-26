package admin

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/crosslink/internal/model"
	"github.com/gin-gonic/gin"
)

// 护栏命中下钻筛选（护栏管理「拦截记录」页签的数据面）：guardrail_triggered 只在
// 精确等于 "true" 时收窄（与 has_fallback 同契约），guardrail_rule 按命中时记录的
// 规则名精确匹配，二者可独立或叠加使用；缺省不改变既有列表语义。
func listUsageIDs(t *testing.T, h *UsageHandler, rawQuery string) (ids []int64, total int64) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/usage?"+rawQuery, nil)
	setAdminContext(c, 1, 1, "admin")
	h.List(c)
	if w.Code != http.StatusOK {
		t.Fatalf("List(%q) status = %d, body = %s", rawQuery, w.Code, w.Body.String())
	}
	var resp struct {
		Data []struct {
			ID int64 `json:"id"`
		} `json:"data"`
		Pagination struct {
			Total int64 `json:"total"`
		} `json:"pagination"`
	}
	decodeResponse(t, w, &resp)
	for _, row := range resp.Data {
		ids = append(ids, row.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, resp.Pagination.Total
}

func TestUsageList_GuardrailFilters(t *testing.T) {
	t.Parallel()
	db := newUsageTestDB(t)
	// 全新内存库：按插入顺序取得自增 ID 1/2/3（mutate 在 Create 之前执行，拿不到 ID）
	const idBlock, idMask, idPlain = 1, 2, 3
	seedUsageLog(t, db, func(l *model.UsageLog) {
		l.RequestID, l.GuardrailTriggered, l.GuardrailRule = "req-block", true, "r-block"
	})
	seedUsageLog(t, db, func(l *model.UsageLog) {
		l.RequestID, l.GuardrailTriggered, l.GuardrailRule = "req-mask", true, "r-mask"
	})
	seedUsageLog(t, db, func(l *model.UsageLog) {
		l.RequestID = "req-plain"
	})
	h := NewUsageHandler(db, "", nil)

	cases := []struct {
		name  string
		query string
		want  []int64
	}{
		{"无筛选：全部行", "", []int64{idBlock, idMask, idPlain}},
		{"guardrail_triggered=true：仅命中行", "guardrail_triggered=true", []int64{idBlock, idMask}},
		{"非 true 值不收窄", "guardrail_triggered=false", []int64{idBlock, idMask, idPlain}},
		{"guardrail_rule 独立生效", "guardrail_rule=r-mask", []int64{idMask}},
		{"叠加：命中且指定规则", "guardrail_triggered=true&guardrail_rule=r-block", []int64{idBlock}},
		{"叠加：无匹配规则名", "guardrail_triggered=true&guardrail_rule=gone", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, total := listUsageIDs(t, h, tc.query)
			if len(got) == 0 && len(tc.want) == 0 {
				if total != 0 {
					t.Fatalf("total = %d, want 0", total)
				}
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ids = %v, want %v (total %d)", got, tc.want, total)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("ids = %v, want %v", got, tc.want)
				}
			}
			if total != int64(len(tc.want)) {
				t.Fatalf("total = %d, want %d (count must see the same filters as the rows)", total, len(tc.want))
			}
		})
	}
}

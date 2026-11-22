package guardrail

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func TestReviewKeepsPublishedSnapshot(t *testing.T) {
	db := setupSeedDB(t)
	require.NoError(t, db.AutoMigrate(&SecurityProfile{}, &SecurityProfileVersion{}, &SecurityProfileBinding{}, &ReviewService{}))
	require.NoError(t, db.Exec("CREATE TABLE api_keys (id integer, org_id integer, status integer, deleted_at datetime)").Error)
	require.NoError(t, db.Exec("INSERT INTO api_keys VALUES (11,7,1,NULL)").Error)
	require.NoError(t, db.Create(&SecurityProfile{ID: 1, OrgID: 7, Published: 1}).Error)
	require.NoError(t, db.Create(&SecurityProfileVersion{ProfileID: 1, Version: 1, ProfileContent: ProfileContent{Name: "客服", Purpose: "查询订单"}}).Error)
	require.NoError(t, db.Create(&SecurityProfileBinding{APIKeyID: 11, ProfileID: 1}).Error)
	var audit bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&audit, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	s := NewGuardrailService(db, nil)
	org := int64(7)
	legacy, finish := s.prepareReviews(t.Context(), &CheckRequest{APIKeyID: 11, OrgID: org, Content: "正文"}, []GuardrailRule{
		{ID: 1, Type: ModelReview, OrgID: &org, Action: "log", Config: datatypes.JSON(`{"service_id":999}`)},
		{ID: 2, Type: "regex"},
	})
	require.Len(t, legacy, 1)
	require.EqualValues(t, 2, legacy[0].ID)
	// 检查中途发布或解绑只能影响下一次请求。
	require.NoError(t, db.Where("api_key_id=11").Delete(&SecurityProfileBinding{}).Error)
	response, err := finish(&CheckResponse{}, nil)
	require.NoError(t, err)
	require.False(t, response.Blocked)
	require.Contains(t, audit.String(), `"profile_id":1,"profile_version":1`)
	require.NotContains(t, audit.String(), "正文")
	require.NotContains(t, audit.String(), "查询订单")
}

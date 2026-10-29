package settings

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseAndValidateEnum(t *testing.T) {
	// kEnum keys (e.g. toolset_granularity) must reject values outside the
	// allowed set — the Validate hook is invoked from the TypeString branch.
	for _, ok := range []string{"name", "name_desc", "full"} {
		s, err := ParseAndValidate(KeyToolsetGran, json.RawMessage(`"`+ok+`"`))
		require.NoError(t, err, ok)
		assert.Equal(t, ok, s)
	}
	_, err := ParseAndValidate(KeyToolsetGran, json.RawMessage(`"bogus"`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), KeyToolsetGran)
	assert.Contains(t, err.Error(), "name|name_desc|full")
}

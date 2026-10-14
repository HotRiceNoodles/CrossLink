package guardrail

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// B5 split: validation is PURE (memory untouched on any input) and Apply
// publishes only pre-validated prefixes, firing observers after the swap.

func TestValidateInternalAllowlist_IsPure(t *testing.T) {
	restoreStrict(t)
	require.NoError(t, SetInternalAllowlist([]string{"10.99.0.0/16"}))
	before := InternalAllowlistSnapshot()

	// Invalid input returns an error and leaves the active policy untouched.
	_, err := ValidateInternalAllowlist([]string{"8.8.8.8/32"})
	require.Error(t, err)
	after := InternalAllowlistSnapshot()
	require.Len(t, after, 1)
	assert.Equal(t, before[0].String(), after[0].String())

	// Red-line CIDRs are rejected by the pure validator exactly as by Set.
	for _, bad := range []string{"127.0.0.0/8", "169.254.0.0/16", "fd00:ec2::/32", "0.0.0.0/8", "224.0.0.0/4", "::ffff:10.0.0.0/104"} {
		_, err := ValidateInternalAllowlist([]string{bad})
		assert.Error(t, err, "pure validator must reject %s", bad)
	}

	// Valid input returns normalized prefixes; still nothing applied.
	prefixes, err := ValidateInternalAllowlist([]string{" 10.30.0.0/16 "})
	require.NoError(t, err)
	require.Len(t, prefixes, 1)
	assert.Equal(t, "10.30.0.0/16", prefixes[0].String())
	assert.Equal(t, "10.99.0.0/16", InternalAllowlistSnapshot()[0].String())
}

func TestApplyInternalAllowlist_PublishesAndNotifies(t *testing.T) {
	restoreStrict(t)
	called := 0
	OnInternalAllowlistApply(func() { called++ })

	prefixes, err := ValidateInternalAllowlist([]string{"10.30.0.0/16"})
	require.NoError(t, err)
	ApplyInternalAllowlist(prefixes)
	assert.Equal(t, 1, called)
	require.Len(t, InternalAllowlistSnapshot(), 1)

	// Empty = strict; observers fire on every publish.
	ApplyInternalAllowlist(nil)
	assert.Equal(t, 2, called)
	assert.Nil(t, InternalAllowlistSnapshot())

	// SetInternalAllowlist (validate+apply) also notifies.
	require.NoError(t, SetInternalAllowlist([]string{"10.40.0.0/16"}))
	assert.Equal(t, 3, called)
}

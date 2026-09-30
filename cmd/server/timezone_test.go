package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestResolveEffectiveTimezone(t *testing.T) {
	// DB row wins.
	assert.Equal(t, "Asia/Shanghai", resolveEffectiveTimezone("Asia/Shanghai", "UTC"))
	// DB row absent → config value.
	assert.Equal(t, "UTC", resolveEffectiveTimezone("", "UTC"))
	// Both absent → local zone name, or "" on the "Local" placeholder.
	got := resolveEffectiveTimezone("", "")
	if name := localZoneName(); name != "" {
		assert.Equal(t, name, got)
	} else {
		assert.Equal(t, "", got)
	}
	// "Local" placeholder and invalid names never leak to the DSN.
	assert.Equal(t, "UTC", resolveEffectiveTimezone("Local", "UTC"))
	assert.Equal(t, "UTC", resolveEffectiveTimezone("Not/AZone", "UTC"))
	assert.Equal(t, "Asia/Shanghai", resolveEffectiveTimezone("Asia/Shanghai", ""))
}

func localZoneName() string {
	name := time.Local.String()
	if name == "" || name == "Local" {
		return ""
	}
	return name
}

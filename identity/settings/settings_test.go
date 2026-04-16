package settings

import (
	"regexp"
	"testing"
	"time"

	"github.com/rudderlabs/rudder-go-kit/config"
	"github.com/stretchr/testify/require"
)

// newTestSettings creates a minimal ResolutionSettings for testing
// without requiring a config.Config dependency. Initializes all internal
// maps and sets a sensible default limit for unconfigured identifier types.
func newTestSettings() *ResolutionSettings {
	return &ResolutionSettings{
		identifiers:     make(map[string]*IdentifierConfig),
		compiledRegexes: make(map[string]*regexp.Regexp),
		defaultLimit:    IdentifierLimit{MaxCount: 5, TimeWindow: "monthly"},
	}
}

// =====================================================================
// Phase 1: Blocked Values Tests
// =====================================================================

func TestBlockedValues_ExactMatch(t *testing.T) {
	t.Parallel()
	s := newTestSettings()
	err := s.SetIdentifierConfig("user_id", &IdentifierConfig{
		BlockedValues: []BlockedValueRule{
			{Type: "exact", Value: "null"},
			{Type: "exact", Value: "undefined"},
			{Type: "exact", Value: "-1"},
			{Type: "exact", Value: "anonymous"},
		},
		Limit:    IdentifierLimit{MaxCount: 1, TimeWindow: "ever"},
		Priority: 1,
	})
	require.NoError(t, err)

	t.Run("blocked exact values are detected", func(t *testing.T) {
		t.Parallel()
		require.True(t, s.IsBlocked("user_id", "null"))
		require.True(t, s.IsBlocked("user_id", "undefined"))
		require.True(t, s.IsBlocked("user_id", "-1"))
		require.True(t, s.IsBlocked("user_id", "anonymous"))
	})

	t.Run("valid values are not blocked", func(t *testing.T) {
		t.Parallel()
		require.False(t, s.IsBlocked("user_id", "validuser123"))
		require.False(t, s.IsBlocked("user_id", "user_abc"))
		require.False(t, s.IsBlocked("user_id", "real_user_42"))
	})

	t.Run("exact match is case sensitive", func(t *testing.T) {
		t.Parallel()
		require.False(t, s.IsBlocked("user_id", "NULL"))
		require.False(t, s.IsBlocked("user_id", "Null"))
		require.False(t, s.IsBlocked("user_id", "UNDEFINED"))
		require.False(t, s.IsBlocked("user_id", "Anonymous"))
	})

	t.Run("empty string is not blocked unless explicitly configured", func(t *testing.T) {
		t.Parallel()
		require.False(t, s.IsBlocked("user_id", ""))
	})
}

func TestBlockedValues_Regex(t *testing.T) {
	t.Parallel()
	s := newTestSettings()
	err := s.SetIdentifierConfig("user_id", &IdentifierConfig{
		BlockedValues: []BlockedValueRule{
			{Type: "regex", Value: "^[0-]*$"},
		},
		Limit:    IdentifierLimit{MaxCount: 1, TimeWindow: "ever"},
		Priority: 1,
	})
	require.NoError(t, err)

	t.Run("regex matches zeroes and dashes patterns", func(t *testing.T) {
		t.Parallel()
		require.True(t, s.IsBlocked("user_id", "000000"))
		require.True(t, s.IsBlocked("user_id", "0-0-0"))
		require.True(t, s.IsBlocked("user_id", "---"))
		require.True(t, s.IsBlocked("user_id", "0"))
		require.True(t, s.IsBlocked("user_id", "-"))
	})

	t.Run("regex does not match valid values", func(t *testing.T) {
		t.Parallel()
		require.False(t, s.IsBlocked("user_id", "abc123"))
		require.False(t, s.IsBlocked("user_id", "0valid"))
		require.False(t, s.IsBlocked("user_id", "valid0"))
		require.False(t, s.IsBlocked("user_id", "hello"))
	})
}

func TestBlockedValues_MultipleRules(t *testing.T) {
	t.Parallel()
	s := newTestSettings()
	err := s.SetIdentifierConfig("user_id", &IdentifierConfig{
		BlockedValues: []BlockedValueRule{
			{Type: "exact", Value: "null"},
			{Type: "exact", Value: "undefined"},
			{Type: "exact", Value: "-1"},
			{Type: "exact", Value: "anonymous"},
			{Type: "regex", Value: "^[0-]*$"},
		},
		Limit:    IdentifierLimit{MaxCount: 1, TimeWindow: "ever"},
		Priority: 1,
	})
	require.NoError(t, err)

	// All exact matches should be blocked
	require.True(t, s.IsBlocked("user_id", "null"))
	require.True(t, s.IsBlocked("user_id", "undefined"))
	require.True(t, s.IsBlocked("user_id", "-1"))
	require.True(t, s.IsBlocked("user_id", "anonymous"))

	// Regex matches should also be blocked
	require.True(t, s.IsBlocked("user_id", "000000"))
	require.True(t, s.IsBlocked("user_id", "0-0-0"))
	require.True(t, s.IsBlocked("user_id", "---"))

	// Valid values pass through all rules
	require.False(t, s.IsBlocked("user_id", "validuser123"))
	require.False(t, s.IsBlocked("user_id", "real_user"))
	require.False(t, s.IsBlocked("user_id", "user@example.com"))
}

func TestBlockedValues_DifferentIdentifiers(t *testing.T) {
	t.Parallel()
	s := newTestSettings()
	err := s.SetIdentifierConfig("user_id", &IdentifierConfig{
		BlockedValues: []BlockedValueRule{
			{Type: "exact", Value: "null"},
		},
		Limit:    IdentifierLimit{MaxCount: 1, TimeWindow: "ever"},
		Priority: 1,
	})
	require.NoError(t, err)

	// user_id has blocked values configured — "null" is blocked
	require.True(t, s.IsBlocked("user_id", "null"))

	// email has NO blocked values configured — "null" is NOT blocked
	require.False(t, s.IsBlocked("email", "null"))

	// anonymous_id has NO blocked values configured
	require.False(t, s.IsBlocked("anonymous_id", "null"))

	// Completely unknown identifier type
	require.False(t, s.IsBlocked("custom_unknown_type", "null"))
}

func TestBlockedValues_InvalidRegex(t *testing.T) {
	t.Parallel()
	s := newTestSettings()

	// SetIdentifierConfig with invalid regex should return an error
	err := s.SetIdentifierConfig("user_id", &IdentifierConfig{
		BlockedValues: []BlockedValueRule{
			{Type: "regex", Value: "[invalid"},
		},
		Limit:    IdentifierLimit{MaxCount: 1, TimeWindow: "ever"},
		Priority: 1,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "regex")

	// Settings should still be safe to use after the error — no panic
	require.False(t, s.IsBlocked("user_id", "anything"))
	require.False(t, s.IsBlocked("user_id", "[invalid"))
}

func TestBlockedValues_EmptyRules(t *testing.T) {
	t.Parallel()
	s := newTestSettings()

	// No identifiers configured at all — nothing should be blocked
	require.False(t, s.IsBlocked("user_id", "anything"))
	require.False(t, s.IsBlocked("email", "test@example.com"))
	require.False(t, s.IsBlocked("anonymous_id", "000"))
	require.False(t, s.IsBlocked("", ""))
}

// =====================================================================
// Phase 2: Identifier Limits Tests
// =====================================================================

func TestIdentifierLimits_Weekly(t *testing.T) {
	t.Parallel()
	s := newTestSettings()
	err := s.SetIdentifierConfig("anonymous_id", &IdentifierConfig{
		Limit:    IdentifierLimit{MaxCount: 5, TimeWindow: "weekly"},
		Priority: 3,
	})
	require.NoError(t, err)

	require.True(t, s.ExceedsLimit("anonymous_id", 5))  // at limit — exceeds
	require.False(t, s.ExceedsLimit("anonymous_id", 4)) // under limit
	require.True(t, s.ExceedsLimit("anonymous_id", 6))  // over limit
	require.False(t, s.ExceedsLimit("anonymous_id", 0)) // zero count
}

func TestIdentifierLimits_Monthly(t *testing.T) {
	t.Parallel()
	s := newTestSettings()
	err := s.SetIdentifierConfig("email", &IdentifierConfig{
		Limit:    IdentifierLimit{MaxCount: 5, TimeWindow: "monthly"},
		Priority: 2,
	})
	require.NoError(t, err)

	require.True(t, s.ExceedsLimit("email", 5))
	require.False(t, s.ExceedsLimit("email", 4))
	require.True(t, s.ExceedsLimit("email", 10))
	require.False(t, s.ExceedsLimit("email", 0))
}

func TestIdentifierLimits_Annually(t *testing.T) {
	t.Parallel()
	s := newTestSettings()
	err := s.SetIdentifierConfig("ios.id", &IdentifierConfig{
		Limit:    IdentifierLimit{MaxCount: 5, TimeWindow: "annually"},
		Priority: 4,
	})
	require.NoError(t, err)

	require.True(t, s.ExceedsLimit("ios.id", 5))
	require.False(t, s.ExceedsLimit("ios.id", 4))
	require.True(t, s.ExceedsLimit("ios.id", 100))
	require.False(t, s.ExceedsLimit("ios.id", 1))
}

func TestIdentifierLimits_Ever(t *testing.T) {
	t.Parallel()
	s := newTestSettings()
	err := s.SetIdentifierConfig("user_id", &IdentifierConfig{
		Limit:    IdentifierLimit{MaxCount: 1, TimeWindow: "ever"},
		Priority: 1,
	})
	require.NoError(t, err)

	// user_id has limit 1 ever — immutable ID, once set should never change
	require.True(t, s.ExceedsLimit("user_id", 1))
	require.False(t, s.ExceedsLimit("user_id", 0))
	require.True(t, s.ExceedsLimit("user_id", 2))
	require.True(t, s.ExceedsLimit("user_id", 100))
}

func TestIdentifierLimits_DefaultLimits(t *testing.T) {
	t.Parallel()
	s := DefaultSettings()

	// Verify user_id: limit 1 ever (Segment default for immutable IDs)
	userLimit := s.GetLimit("user_id")
	require.Equal(t, 1, userLimit.MaxCount)
	require.Equal(t, "ever", userLimit.TimeWindow)

	// Verify email has a configured limit with valid time window
	emailLimit := s.GetLimit("email")
	require.Positive(t, emailLimit.MaxCount)
	require.True(t, ValidTimeWindows[emailLimit.TimeWindow])

	// user_id at count 1 exceeds its limit of 1
	require.True(t, s.ExceedsLimit("user_id", 1))
	require.False(t, s.ExceedsLimit("user_id", 0))
}

func TestIdentifierLimits_UnconfiguredIdentifier(t *testing.T) {
	t.Parallel()
	s := newTestSettings()
	err := s.SetIdentifierConfig("user_id", &IdentifierConfig{
		Limit:    IdentifierLimit{MaxCount: 1, TimeWindow: "ever"},
		Priority: 1,
	})
	require.NoError(t, err)

	// custom_id has no explicit limit — should fall back to default limit (5 monthly)
	customLimit := s.GetLimit("custom_id")
	require.Equal(t, s.defaultLimit.MaxCount, customLimit.MaxCount)
	require.Equal(t, s.defaultLimit.TimeWindow, customLimit.TimeWindow)

	// Verify default limit behavior: MaxCount is 5
	require.False(t, s.ExceedsLimit("custom_id", 4))
	require.True(t, s.ExceedsLimit("custom_id", 5))
}

func TestTimeWindow_Validation(t *testing.T) {
	t.Parallel()

	t.Run("valid time windows", func(t *testing.T) {
		t.Parallel()
		validWindows := []string{"weekly", "monthly", "annually", "ever"}
		for _, window := range validWindows {
			require.True(t, ValidTimeWindows[window], "%s should be a valid time window", window)
		}
		require.Len(t, ValidTimeWindows, 4)
	})

	t.Run("invalid time windows", func(t *testing.T) {
		t.Parallel()
		invalidWindows := []string{"daily", "hourly", "", "biweekly", "quarterly", "minutely"}
		for _, window := range invalidWindows {
			require.False(t, ValidTimeWindows[window], "%s should NOT be a valid time window", window)
		}
	})
}

func TestTimeWindow_Duration(t *testing.T) {
	t.Parallel()

	t.Run("weekly returns 7 days", func(t *testing.T) {
		t.Parallel()
		require.Equal(t, 7*24*time.Hour, GetWindowDuration("weekly"))
	})

	t.Run("monthly returns 30 days", func(t *testing.T) {
		t.Parallel()
		require.Equal(t, 30*24*time.Hour, GetWindowDuration("monthly"))
	})

	t.Run("annually returns 365 days", func(t *testing.T) {
		t.Parallel()
		require.Equal(t, 365*24*time.Hour, GetWindowDuration("annually"))
	})

	t.Run("ever returns zero duration (lifetime)", func(t *testing.T) {
		t.Parallel()
		require.Zero(t, GetWindowDuration("ever"))
	})

	t.Run("unknown window returns zero", func(t *testing.T) {
		t.Parallel()
		require.Zero(t, GetWindowDuration("unknown"))
	})
}

// =====================================================================
// Phase 3: Priority Ranking Tests
// =====================================================================

func TestPriority_DefaultOrder(t *testing.T) {
	t.Parallel()
	s := DefaultSettings()

	// user_id should be highest priority (1)
	require.Equal(t, 1, s.GetPriority("user_id"))

	// email should be second priority (2)
	require.Equal(t, 2, s.GetPriority("email"))

	// user_id has higher priority (lower number) than email
	require.Less(t, s.GetPriority("user_id"), s.GetPriority("email"))

	// All remaining default identifiers have priority >= 3
	for _, idType := range DefaultExternalIDTypes {
		if idType == "user_id" || idType == "email" {
			continue
		}
		priority := s.GetPriority(idType)
		require.Greater(t, priority, 2, "%s should have priority > 2", idType)
	}
}

func TestPriority_CustomOrder(t *testing.T) {
	t.Parallel()
	s := newTestSettings()

	configs := []struct {
		idType   string
		priority int
	}{
		{"user_id", 1},
		{"email", 2},
		{"anonymous_id", 3},
		{"ios.id", 4},
	}
	for _, c := range configs {
		err := s.SetIdentifierConfig(c.idType, &IdentifierConfig{
			Limit:    IdentifierLimit{MaxCount: 5, TimeWindow: "monthly"},
			Priority: c.priority,
		})
		require.NoError(t, err)
	}

	// Verify explicit priorities
	require.Equal(t, 1, s.GetPriority("user_id"))
	require.Equal(t, 2, s.GetPriority("email"))
	require.Equal(t, 3, s.GetPriority("anonymous_id"))
	require.Equal(t, 4, s.GetPriority("ios.id"))

	// CompareIdentifierPriority: user_id (1) vs email (2) → negative (user_id is higher priority)
	cmp := s.CompareIdentifierPriority("user_id", "email")
	require.Negative(t, cmp)

	// email (2) vs user_id (1) → positive (email is lower priority)
	cmp = s.CompareIdentifierPriority("email", "user_id")
	require.Positive(t, cmp)

	// Same identifier → zero
	require.Zero(t, s.CompareIdentifierPriority("email", "email"))
	require.Zero(t, s.CompareIdentifierPriority("user_id", "user_id"))
}

func TestPriority_UnconfiguredIdentifier(t *testing.T) {
	t.Parallel()
	s := newTestSettings()
	err := s.SetIdentifierConfig("user_id", &IdentifierConfig{
		Limit:    IdentifierLimit{MaxCount: 1, TimeWindow: "ever"},
		Priority: 1,
	})
	require.NoError(t, err)
	err = s.SetIdentifierConfig("email", &IdentifierConfig{
		Limit:    IdentifierLimit{MaxCount: 5, TimeWindow: "annually"},
		Priority: 2,
	})
	require.NoError(t, err)

	// Unconfigured identifier should have priority higher (numerically greater)
	// than all configured identifiers
	zzPriority := s.GetPriority("zzz_custom")
	require.Greater(t, zzPriority, s.GetPriority("user_id"))
	require.Greater(t, zzPriority, s.GetPriority("email"))

	// Multiple unconfigured identifiers maintain alphabetical ordering:
	// "abc_custom" alphabetically < "zzz_custom", so abc has lower numeric priority
	abcPriority := s.GetPriority("abc_custom")
	require.Greater(t, abcPriority, s.GetPriority("email"))
	require.Less(t, abcPriority, zzPriority)
}

func TestPriority_HigherPriorityWins(t *testing.T) {
	t.Parallel()
	s := newTestSettings()

	// Setup: user_id priority 1, email priority 2, anonymous_id priority 3
	// Matching the Segment documentation example
	err := s.SetIdentifierConfig("user_id", &IdentifierConfig{
		Limit:    IdentifierLimit{MaxCount: 1, TimeWindow: "ever"},
		Priority: 1,
	})
	require.NoError(t, err)
	err = s.SetIdentifierConfig("email", &IdentifierConfig{
		Limit:    IdentifierLimit{MaxCount: 5, TimeWindow: "annually"},
		Priority: 2,
	})
	require.NoError(t, err)
	err = s.SetIdentifierConfig("anonymous_id", &IdentifierConfig{
		Limit:    IdentifierLimit{MaxCount: 5, TimeWindow: "weekly"},
		Priority: 3,
	})
	require.NoError(t, err)

	// email (priority 2) should be demoted in favor of user_id (priority 1)
	require.True(t, s.ShouldDemoteIdentifier("email", "user_id"))

	// anonymous_id (priority 3) should be demoted in favor of user_id (priority 1)
	require.True(t, s.ShouldDemoteIdentifier("anonymous_id", "user_id"))

	// anonymous_id (priority 3) should be demoted in favor of email (priority 2)
	require.True(t, s.ShouldDemoteIdentifier("anonymous_id", "email"))

	// user_id (priority 1) should NOT be demoted in favor of email (priority 2)
	require.False(t, s.ShouldDemoteIdentifier("user_id", "email"))

	// user_id should NOT be demoted in favor of anonymous_id
	require.False(t, s.ShouldDemoteIdentifier("user_id", "anonymous_id"))

	// Same identifier should NOT be demoted (equal priority → not strictly greater)
	require.False(t, s.ShouldDemoteIdentifier("user_id", "user_id"))
	require.False(t, s.ShouldDemoteIdentifier("email", "email"))
}

// =====================================================================
// Phase 4: ResolutionSettings Integration Tests
// =====================================================================

func TestResolutionSettings_NewFromConfig(t *testing.T) {
	t.Parallel()
	s := newTestSettings()

	configMap := map[string]any{
		"identifiers": map[string]any{
			"user_id": map[string]any{
				"blockedValues": []any{
					map[string]any{"type": "exact", "value": "null"},
					map[string]any{"type": "regex", "value": "^[0-]*$"},
				},
				"limit": map[string]any{
					"maxCount":   1,
					"timeWindow": "ever",
				},
				"priority": 1,
			},
			"email": map[string]any{
				"blockedValues": []any{},
				"limit": map[string]any{
					"maxCount":   5,
					"timeWindow": "annually",
				},
				"priority": 2,
			},
		},
	}

	err := s.LoadFromConfig(configMap)
	require.NoError(t, err)

	// Verify user_id blocking
	require.True(t, s.IsBlocked("user_id", "null"))
	require.True(t, s.IsBlocked("user_id", "000"))
	require.False(t, s.IsBlocked("user_id", "real_user"))

	// Verify user_id priority and limit
	require.Equal(t, 1, s.GetPriority("user_id"))
	userLimit := s.GetLimit("user_id")
	require.Equal(t, 1, userLimit.MaxCount)
	require.Equal(t, "ever", userLimit.TimeWindow)

	// Verify email config
	require.Equal(t, 2, s.GetPriority("email"))
	emailLimit := s.GetLimit("email")
	require.Equal(t, 5, emailLimit.MaxCount)
	require.Equal(t, "annually", emailLimit.TimeWindow)
}

func TestResolutionSettings_Validate(t *testing.T) {
	t.Parallel()

	t.Run("valid default settings pass validation", func(t *testing.T) {
		t.Parallel()
		s := DefaultSettings()
		err := s.Validate()
		require.NoError(t, err)
	})

	t.Run("custom valid settings pass validation", func(t *testing.T) {
		t.Parallel()
		s := newTestSettings()
		err := s.SetIdentifierConfig("user_id", &IdentifierConfig{
			BlockedValues: []BlockedValueRule{
				{Type: "exact", Value: "null"},
				{Type: "regex", Value: "^[0-]*$"},
			},
			Limit:    IdentifierLimit{MaxCount: 1, TimeWindow: "ever"},
			Priority: 1,
		})
		require.NoError(t, err)

		err = s.Validate()
		require.NoError(t, err)
	})

	t.Run("invalid regex returns error", func(t *testing.T) {
		t.Parallel()
		s := newTestSettings()
		// Directly set invalid state bypassing SetIdentifierConfig validation
		s.identifiers["user_id"] = &IdentifierConfig{
			BlockedValues: []BlockedValueRule{
				{Type: "regex", Value: "[invalid"},
			},
			Limit:    IdentifierLimit{MaxCount: 1, TimeWindow: "ever"},
			Priority: 1,
		}
		err := s.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "regex")
	})

	t.Run("invalid time window returns error", func(t *testing.T) {
		t.Parallel()
		s := newTestSettings()
		s.identifiers["user_id"] = &IdentifierConfig{
			Limit:    IdentifierLimit{MaxCount: 5, TimeWindow: "daily"},
			Priority: 1,
		}
		err := s.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "time window")
	})

	t.Run("negative limit returns error", func(t *testing.T) {
		t.Parallel()
		s := newTestSettings()
		s.identifiers["user_id"] = &IdentifierConfig{
			Limit:    IdentifierLimit{MaxCount: -1, TimeWindow: "monthly"},
			Priority: 1,
		}
		err := s.Validate()
		require.Error(t, err)
	})

	t.Run("zero limit returns error", func(t *testing.T) {
		t.Parallel()
		s := newTestSettings()
		s.identifiers["user_id"] = &IdentifierConfig{
			Limit:    IdentifierLimit{MaxCount: 0, TimeWindow: "monthly"},
			Priority: 1,
		}
		err := s.Validate()
		require.Error(t, err)
	})

	t.Run("zero priority returns error", func(t *testing.T) {
		t.Parallel()
		s := newTestSettings()
		s.identifiers["user_id"] = &IdentifierConfig{
			Limit:    IdentifierLimit{MaxCount: 5, TimeWindow: "monthly"},
			Priority: 0,
		}
		err := s.Validate()
		require.Error(t, err)
	})
}

func TestResolutionSettings_DefaultSettings(t *testing.T) {
	t.Parallel()
	s := DefaultSettings()
	require.NotNil(t, s)

	// Verify all 12+ default external ID types have priority entries
	expectedTypes := []string{
		"user_id", "email", "anonymous_id",
		"android.id", "android.idfa", "android.push_token",
		"ios.id", "ios.idfa", "ios.push_token",
		"ga_client_id", "cross_domain_id", "braze_id",
	}
	for _, idType := range expectedTypes {
		cfg := s.GetIdentifierConfig(idType)
		require.Positive(t, cfg.Priority, "priority should be positive for %s", idType)
	}

	// Verify user_id: priority 1, limit 1 ever
	require.Equal(t, 1, s.GetPriority("user_id"))
	userLimit := s.GetLimit("user_id")
	require.Equal(t, 1, userLimit.MaxCount)
	require.Equal(t, "ever", userLimit.TimeWindow)

	// Verify email: priority 2
	require.Equal(t, 2, s.GetPriority("email"))

	// Verify default blocked values are applied to user_id
	s.mu.RLock()
	userCfg, exists := s.identifiers["user_id"]
	s.mu.RUnlock()
	require.True(t, exists)
	require.NotNil(t, userCfg.BlockedValues)

	// Check Segment-recommended defaults are present
	hasRegex := false
	hasExactNull := false
	hasExactMinusOne := false
	hasExactAnonymous := false
	for _, rule := range userCfg.BlockedValues {
		switch {
		case rule.Type == "regex" && rule.Value == "^[0-]*$":
			hasRegex = true
		case rule.Type == "exact" && rule.Value == "null":
			hasExactNull = true
		case rule.Type == "exact" && rule.Value == "-1":
			hasExactMinusOne = true
		case rule.Type == "exact" && rule.Value == "anonymous":
			hasExactAnonymous = true
		}
	}
	require.True(t, hasRegex, "should have regex blocked value ^[0-]*$")
	require.True(t, hasExactNull, "should have exact blocked value 'null'")
	require.True(t, hasExactMinusOne, "should have exact blocked value '-1'")
	require.True(t, hasExactAnonymous, "should have exact blocked value 'anonymous'")

	// Validate the default settings are fully valid
	err := s.Validate()
	require.NoError(t, err)
}

func TestResolutionSettings_GetIdentifierConfig(t *testing.T) {
	t.Parallel()
	s := DefaultSettings()

	t.Run("configured identifier returns full config", func(t *testing.T) {
		t.Parallel()
		cfg := s.GetIdentifierConfig("user_id")
		require.NotNil(t, cfg.BlockedValues)
		require.Positive(t, cfg.Limit.MaxCount)
		require.Equal(t, 1, cfg.Priority)
		require.True(t, ValidTimeWindows[cfg.Limit.TimeWindow])
	})

	t.Run("unconfigured identifier returns default config", func(t *testing.T) {
		t.Parallel()
		cfg := s.GetIdentifierConfig("unknown_type")
		require.Nil(t, cfg.BlockedValues)
		require.Positive(t, cfg.Limit.MaxCount)
		require.Positive(t, cfg.Priority)
	})
}

// =====================================================================
// Phase 5: Compiled Regex Caching Tests
// =====================================================================

func TestBlockedValues_RegexCaching(t *testing.T) {
	t.Parallel()
	s := newTestSettings()
	err := s.SetIdentifierConfig("user_id", &IdentifierConfig{
		BlockedValues: []BlockedValueRule{
			{Type: "regex", Value: "^[0-]*$"},
			{Type: "regex", Value: "^test_"},
		},
		Limit:    IdentifierLimit{MaxCount: 1, TimeWindow: "ever"},
		Priority: 1,
	})
	require.NoError(t, err)

	// Verify regex patterns are compiled and cached in the internal map
	s.mu.RLock()
	cachedRegex1, exists1 := s.compiledRegexes["^[0-]*$"]
	cachedRegex2, exists2 := s.compiledRegexes["^test_"]
	s.mu.RUnlock()

	require.True(t, exists1, "first regex pattern should be cached")
	require.True(t, exists2, "second regex pattern should be cached")
	require.NotNil(t, cachedRegex1)
	require.NotNil(t, cachedRegex2)

	// Call IsBlocked many times to verify caching stability (no panic, consistent results)
	for i := 0; i < 100; i++ {
		require.True(t, s.IsBlocked("user_id", "000"))
		require.True(t, s.IsBlocked("user_id", "test_something"))
		require.False(t, s.IsBlocked("user_id", "valid_user"))
	}

	// Verify same regex instances are still cached after many calls
	s.mu.RLock()
	cachedAfter1 := s.compiledRegexes["^[0-]*$"]
	cachedAfter2 := s.compiledRegexes["^test_"]
	cacheSize := len(s.compiledRegexes)
	s.mu.RUnlock()

	require.Equal(t, cachedRegex1, cachedAfter1, "cached regex should be the same instance")
	require.Equal(t, cachedRegex2, cachedAfter2, "cached regex should be the same instance")
	require.Equal(t, 2, cacheSize, "cache should have exactly 2 entries")
}

// =====================================================================
// Additional: DefaultBlockedValues and DefaultExternalIDTypes Tests
// =====================================================================

func TestDefaultBlockedValues(t *testing.T) {
	t.Parallel()
	rules := DefaultBlockedValues()
	require.NotNil(t, rules)
	require.Len(t, rules, 4) // 1 regex + 3 exact matches

	foundRegex := false
	foundExactMinusOne := false
	foundExactNull := false
	foundExactAnonymous := false
	for _, rule := range rules {
		switch {
		case rule.Type == "regex" && rule.Value == "^[0-]*$":
			foundRegex = true
		case rule.Type == "exact" && rule.Value == "-1":
			foundExactMinusOne = true
		case rule.Type == "exact" && rule.Value == "null":
			foundExactNull = true
		case rule.Type == "exact" && rule.Value == "anonymous":
			foundExactAnonymous = true
		}
	}
	require.True(t, foundRegex, "should include regex for zeroes and dashes")
	require.True(t, foundExactMinusOne, "should include exact match for '-1'")
	require.True(t, foundExactNull, "should include exact match for 'null'")
	require.True(t, foundExactAnonymous, "should include exact match for 'anonymous'")
}

func TestDefaultExternalIDTypes(t *testing.T) {
	t.Parallel()
	require.Len(t, DefaultExternalIDTypes, 12)
	require.Contains(t, DefaultExternalIDTypes, "user_id")
	require.Contains(t, DefaultExternalIDTypes, "email")
	require.Contains(t, DefaultExternalIDTypes, "anonymous_id")
	require.Contains(t, DefaultExternalIDTypes, "android.id")
	require.Contains(t, DefaultExternalIDTypes, "android.idfa")
	require.Contains(t, DefaultExternalIDTypes, "android.push_token")
	require.Contains(t, DefaultExternalIDTypes, "ios.id")
	require.Contains(t, DefaultExternalIDTypes, "ios.idfa")
	require.Contains(t, DefaultExternalIDTypes, "ios.push_token")
	require.Contains(t, DefaultExternalIDTypes, "ga_client_id")
	require.Contains(t, DefaultExternalIDTypes, "cross_domain_id")
	require.Contains(t, DefaultExternalIDTypes, "braze_id")
}

// TestNew_WithConfig verifies the New() constructor properly initializes
// settings from a config.Config instance, reading default limit overrides
// from the config key prefix "Identity.Resolution".
func TestNew_WithConfig(t *testing.T) {
	t.Parallel()

	t.Run("nil config uses defaults", func(t *testing.T) {
		t.Parallel()
		s := New(nil)
		require.NotNil(t, s)
		require.NotNil(t, s.identifiers)
		require.NotNil(t, s.compiledRegexes)
		require.Equal(t, 5, s.defaultLimit.MaxCount)
		require.Equal(t, "monthly", s.defaultLimit.TimeWindow)
	})

	t.Run("with config reads overrides", func(t *testing.T) {
		t.Parallel()
		conf := config.New()
		s := New(conf)
		require.NotNil(t, s)
		require.NotNil(t, s.identifiers)
		require.NotNil(t, s.compiledRegexes)
		// Default config should still return defaults since no overrides are set
		require.Equal(t, 5, s.defaultLimit.MaxCount)
		require.Equal(t, "monthly", s.defaultLimit.TimeWindow)
	})
}

// TestCompileAndCacheRegex exercises the internal compileAndCacheRegex helper
// method which compiles a regex pattern and caches it for subsequent lookups.
func TestCompileAndCacheRegex(t *testing.T) {
	t.Parallel()

	t.Run("compiles and caches valid regex", func(t *testing.T) {
		t.Parallel()
		s := &ResolutionSettings{
			identifiers:     make(map[string]*IdentifierConfig),
			compiledRegexes: make(map[string]*regexp.Regexp),
			defaultLimit:    IdentifierLimit{MaxCount: 5, TimeWindow: "monthly"},
		}

		compiled, err := s.compileAndCacheRegex(`^test\d+$`)
		require.NoError(t, err)
		require.NotNil(t, compiled)
		require.True(t, compiled.MatchString("test123"))
		require.False(t, compiled.MatchString("notmatch"))

		// Second call should return cached version
		compiled2, err := s.compileAndCacheRegex(`^test\d+$`)
		require.NoError(t, err)
		require.NotNil(t, compiled2)
		// Same pointer — cached
		require.Equal(t, compiled, compiled2)
	})

	t.Run("returns error for invalid regex", func(t *testing.T) {
		t.Parallel()
		s := &ResolutionSettings{
			identifiers:     make(map[string]*IdentifierConfig),
			compiledRegexes: make(map[string]*regexp.Regexp),
			defaultLimit:    IdentifierLimit{MaxCount: 5, TimeWindow: "monthly"},
		}

		compiled, err := s.compileAndCacheRegex(`[invalid`)
		require.Error(t, err)
		require.Nil(t, compiled)
		require.Contains(t, err.Error(), "invalid regex pattern")
	})
}

// TestAnyToConversions covers the anyToString and anyToInt helper functions
// including all type branches and edge cases.
func TestAnyToConversions(t *testing.T) {
	t.Parallel()

	t.Run("anyToString", func(t *testing.T) {
		t.Parallel()
		// String value returns the string
		require.Equal(t, "hello", anyToString("hello"))
		// Non-string returns empty string
		require.Equal(t, "", anyToString(42))
		require.Equal(t, "", anyToString(nil))
		require.Equal(t, "", anyToString(true))
		require.Equal(t, "", anyToString(3.14))
	})

	t.Run("anyToInt", func(t *testing.T) {
		t.Parallel()
		// int value returns the int
		require.Equal(t, 42, anyToInt(42))
		// float64 value returns truncated int (common in JSON)
		require.Equal(t, 5, anyToInt(float64(5.9)))
		// int64 value returns the int
		require.Equal(t, 10, anyToInt(int64(10)))
		// Other types return 0
		require.Equal(t, 0, anyToInt("notanumber"))
		require.Equal(t, 0, anyToInt(nil))
		require.Equal(t, 0, anyToInt(true))
	})
}

// TestLoadFromConfig_ErrorPaths exercises all error branches in LoadFromConfig
// that are not covered by the main integration tests.
func TestLoadFromConfig_ErrorPaths(t *testing.T) {
	t.Parallel()

	t.Run("invalid identifiers type returns error", func(t *testing.T) {
		t.Parallel()
		s := DefaultSettings()
		err := s.LoadFromConfig(map[string]any{
			"identifiers": "not_a_map",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "expected map")
	})

	t.Run("invalid identifier config type returns error", func(t *testing.T) {
		t.Parallel()
		s := DefaultSettings()
		err := s.LoadFromConfig(map[string]any{
			"identifiers": map[string]any{
				"user_id": "not_a_map",
			},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "expected map")
	})

	t.Run("invalid blockedValues type returns error", func(t *testing.T) {
		t.Parallel()
		s := DefaultSettings()
		err := s.LoadFromConfig(map[string]any{
			"identifiers": map[string]any{
				"user_id": map[string]any{
					"blockedValues": "not_an_array",
					"limit":         map[string]any{"maxCount": 5, "timeWindow": "monthly"},
					"priority":      1,
				},
			},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "blockedValues must be an array")
	})

	t.Run("non-map blockedValue items are skipped", func(t *testing.T) {
		t.Parallel()
		s := DefaultSettings()
		err := s.LoadFromConfig(map[string]any{
			"identifiers": map[string]any{
				"user_id": map[string]any{
					"blockedValues": []any{"not_a_map_item"},
					"limit":         map[string]any{"maxCount": 5, "timeWindow": "monthly"},
					"priority":      1,
				},
			},
		})
		require.NoError(t, err)
	})

	t.Run("invalid time window in LoadFromConfig returns error", func(t *testing.T) {
		t.Parallel()
		s := DefaultSettings()
		err := s.LoadFromConfig(map[string]any{
			"identifiers": map[string]any{
				"user_id": map[string]any{
					"limit":    map[string]any{"maxCount": 5, "timeWindow": "hourly"},
					"priority": 1,
				},
			},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid time window")
	})

	t.Run("zero priority in LoadFromConfig returns error", func(t *testing.T) {
		t.Parallel()
		s := DefaultSettings()
		err := s.LoadFromConfig(map[string]any{
			"identifiers": map[string]any{
				"user_id": map[string]any{
					"limit":    map[string]any{"maxCount": 5, "timeWindow": "monthly"},
					"priority": 0,
				},
			},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "priority must be positive")
	})

	t.Run("zero maxCount in LoadFromConfig returns error", func(t *testing.T) {
		t.Parallel()
		s := DefaultSettings()
		err := s.LoadFromConfig(map[string]any{
			"identifiers": map[string]any{
				"user_id": map[string]any{
					"limit":    map[string]any{"maxCount": 0, "timeWindow": "monthly"},
					"priority": 1,
				},
			},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "maxCount must be positive")
	})

	t.Run("invalid regex in LoadFromConfig returns error", func(t *testing.T) {
		t.Parallel()
		s := DefaultSettings()
		err := s.LoadFromConfig(map[string]any{
			"identifiers": map[string]any{
				"user_id": map[string]any{
					"blockedValues": []any{
						map[string]any{"type": "regex", "value": "[invalid"},
					},
					"limit":    map[string]any{"maxCount": 5, "timeWindow": "monthly"},
					"priority": 1,
				},
			},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid regex pattern")
	})

	t.Run("no identifiers key is a no-op", func(t *testing.T) {
		t.Parallel()
		s := DefaultSettings()
		err := s.LoadFromConfig(map[string]any{
			"otherKey": "value",
		})
		require.NoError(t, err)
	})

	t.Run("float64 values from JSON are handled", func(t *testing.T) {
		t.Parallel()
		s := DefaultSettings()
		err := s.LoadFromConfig(map[string]any{
			"identifiers": map[string]any{
				"user_id": map[string]any{
					"limit":    map[string]any{"maxCount": float64(3), "timeWindow": "weekly"},
					"priority": float64(1),
				},
			},
		})
		require.NoError(t, err)
		require.Equal(t, 3, s.GetLimit("user_id").MaxCount)
		require.Equal(t, "weekly", s.GetLimit("user_id").TimeWindow)
		require.Equal(t, 1, s.GetPriority("user_id"))
	})
}

// TestSetIdentifierConfig_ErrorPaths exercises additional error branches in
// SetIdentifierConfig.
func TestSetIdentifierConfig_ErrorPaths(t *testing.T) {
	t.Parallel()

	t.Run("invalid time window returns error", func(t *testing.T) {
		t.Parallel()
		s := DefaultSettings()
		err := s.SetIdentifierConfig("user_id", &IdentifierConfig{
			Priority: 1,
			Limit:    IdentifierLimit{MaxCount: 5, TimeWindow: "daily"},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid time window")
	})

	t.Run("invalid regex in blocked values returns error", func(t *testing.T) {
		t.Parallel()
		s := DefaultSettings()
		err := s.SetIdentifierConfig("user_id", &IdentifierConfig{
			Priority: 1,
			Limit:    IdentifierLimit{MaxCount: 5, TimeWindow: "monthly"},
			BlockedValues: []BlockedValueRule{
				{Type: "regex", Value: "[invalid"},
			},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid regex pattern")
	})

	t.Run("successful set with regex compiles and caches", func(t *testing.T) {
		t.Parallel()
		s := DefaultSettings()
		err := s.SetIdentifierConfig("custom_id", &IdentifierConfig{
			Priority: 5,
			Limit:    IdentifierLimit{MaxCount: 10, TimeWindow: "weekly"},
			BlockedValues: []BlockedValueRule{
				{Type: "exact", Value: "blocked"},
				{Type: "regex", Value: `^\d+$`},
			},
		})
		require.NoError(t, err)

		// Verify the regex was compiled and cached
		require.True(t, s.IsBlocked("custom_id", "blocked"))
		require.True(t, s.IsBlocked("custom_id", "12345"))
		require.False(t, s.IsBlocked("custom_id", "valid_user"))
	})
}

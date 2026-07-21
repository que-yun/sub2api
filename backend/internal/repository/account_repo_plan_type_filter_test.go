package repository

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAccountPlanTypeFilterVariantsSuperGrok(t *testing.T) {
	variants := accountPlanTypeFilterVariants("SuperGrok")
	require.Contains(t, variants, "SuperGrok")
	require.Contains(t, variants, "supergrok")
	require.Contains(t, variants, "super_grok")
}

func TestAccountPlanTypeFilterVariantsFreeAliases(t *testing.T) {
	variants := accountPlanTypeFilterVariants("free")
	require.Contains(t, variants, "free")
	require.Contains(t, variants, "basic")
	require.Contains(t, variants, "FREE")
}

func TestAccountPlanTypeFilterVariantsProAliases(t *testing.T) {
	variants := accountPlanTypeFilterVariants("pro")
	require.Contains(t, variants, "pro")
	require.Contains(t, variants, "chatgptpro")
}

func TestAccountPlanTypeFilterVariantsEmpty(t *testing.T) {
	require.Nil(t, accountPlanTypeFilterVariants("   "))
}

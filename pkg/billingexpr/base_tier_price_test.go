package billingexpr

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBaseTierUnitPrices(t *testing.T) {
	input, output, cache, ok, err := BaseTierUnitPrices(`len <= 200000 ? tier("standard", p * 4 + c * 16 + cr * 0.4) : tier("base", p * 2 + c * 8 + cr * 0.2)`)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 2.0, input)
	require.Equal(t, 8.0, output)
	require.Equal(t, 0.2, cache)
}

func TestBaseTierUnitPricesUsesInputPriceForUnconfiguredCache(t *testing.T) {
	input, output, cache, ok, err := BaseTierUnitPrices(`tier("base", p * 2 + c * 8)`)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 2.0, input)
	require.Equal(t, 8.0, output)
	require.Equal(t, input, cache)
}

func TestBaseTierUnitPricesUsesFirstTierWithoutBase(t *testing.T) {
	input, output, cache, ok, err := BaseTierUnitPrices(`len <= 200000 ? tier("standard", p * 3 + c * 15 + cr * 0.3) : tier("long_context", p * 6 + c * 22.5 + cr * 0.6)`)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 3.0, input)
	require.Equal(t, 15.0, output)
	require.Equal(t, 0.3, cache)
}

package gateway

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewGatewayCommand(t *testing.T) {
	cmd := NewGatewayCommand()
	require.NotNil(t, cmd)
	assert.Equal(t, "gateway", cmd.Use)
	assert.Equal(t, "启动 chagent gateway", cmd.Short)
	assert.Empty(t, cmd.Aliases)
	assert.NotNil(t, cmd.RunE)
	assert.False(t, cmd.HasSubCommands())
	assert.NotNil(t, cmd.Flags().Lookup("name"))
	// Legacy flags were removed: config.json is the only behavior source.
	assert.Nil(t, cmd.Flags().Lookup("debug"))
	assert.Nil(t, cmd.Flags().Lookup("allow-empty"))
	assert.Nil(t, cmd.Flags().Lookup("host"))
	assert.Nil(t, cmd.Flags().Lookup("no-truncate"))
}

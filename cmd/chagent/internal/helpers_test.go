package internal

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rlds/chagent/pkg"
	"github.com/rlds/chagent/pkg/config"
)

func TestGetConfigPath(t *testing.T) {
	t.Setenv("HOME", "/tmp/home")

	got := GetConfigPath()
	want := filepath.Join("/tmp/home", pkg.DefaultChagentHome, "config.json")

	assert.Equal(t, want, got)
}

func TestGetConfigPath_WithCHAGENT_HOME(t *testing.T) {
	t.Setenv(config.EnvHome, "/custom/chagent")
	t.Setenv("HOME", "/tmp/home")

	got := GetConfigPath()
	want := filepath.Join("/custom/chagent", "config.json")

	assert.Equal(t, want, got)
}

func TestGetConfigPath_WithCHAGENT_CONFIG(t *testing.T) {
	t.Setenv("CHAGENT_CONFIG", "/custom/config.json")
	t.Setenv(config.EnvHome, "/custom/chagent")
	t.Setenv("HOME", "/tmp/home")

	got := GetConfigPath()
	want := "/custom/config.json"

	assert.Equal(t, want, got)
}

func TestGetConfigPath_Windows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-specific HOME behavior varies; run on windows")
	}

	testUserProfilePath := `C:\Users\Test`
	t.Setenv("USERPROFILE", testUserProfilePath)

	got := GetConfigPath()
	want := filepath.Join(testUserProfilePath, pkg.DefaultChagentHome, "config.json")

	require.True(t, strings.EqualFold(got, want), "GetConfigPath() = %q, want %q", got, want)
}

//go:build testing

package health

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHealth(t *testing.T) {
	dataDir := t.TempDir()

	t.Run("check with no health file", func(t *testing.T) {
		err := Check(dataDir)
		require.Error(t, err)
		assert.True(t, os.IsNotExist(err), "expected a file-not-exist error, but got: %v", err)
	})

	t.Run("update and check", func(t *testing.T) {
		err := Update(dataDir)
		require.NoError(t, err, "Update() failed")

		err = Check(dataDir)
		assert.NoError(t, err, "Check() failed immediately after Update()")
	})

	t.Run("health file lives in the data dir and is private", func(t *testing.T) {
		require.NoError(t, Update(dataDir))
		info, err := os.Lstat(filepath.Join(dataDir, healthFileName))
		require.NoError(t, err)
		assert.True(t, info.Mode().IsRegular())
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	})

	// This test uses synctest to simulate time passing.
	t.Run("check with simulated time", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			// Update the file to set the initial timestamp.
			require.NoError(t, Update(dataDir), "Update() failed inside synctest")

			// Set the mtime to the current fake time to align the file's timestamp with the simulated clock.
			now := time.Now()
			require.NoError(t, os.Chtimes(filepath.Join(dataDir, healthFileName), now, now), "Chtimes failed")

			// Wait a duration less than the threshold.
			time.Sleep(89 * time.Second)
			synctest.Wait()

			// The check should still pass.
			assert.NoError(t, Check(dataDir), "Check() failed after 89s")

			// Wait for the total duration to exceed the threshold.
			time.Sleep(5 * time.Second)
			synctest.Wait()

			// The check should now fail as unhealthy.
			err := Check(dataDir)
			require.Error(t, err, "Check() should have failed after 91s")
			assert.Equal(t, "unhealthy", err.Error(), "Check() returned wrong error")
		})
	})

	t.Run("cleanup removes the file", func(t *testing.T) {
		require.NoError(t, Update(dataDir))
		require.NoError(t, CleanUp(dataDir))
		_, err := os.Lstat(filepath.Join(dataDir, healthFileName))
		assert.True(t, os.IsNotExist(err))
	})
}

// A symlink planted with the health file's name must never make the agent truncate its target:
// the agent runs as root on the Macs, where protected_symlinks does not exist.
func TestHealthRejectsSymlink(t *testing.T) {
	dataDir := t.TempDir()
	target := filepath.Join(t.TempDir(), "victim")
	require.NoError(t, os.WriteFile(target, []byte("keep me"), 0o600))
	require.NoError(t, os.Symlink(target, filepath.Join(dataDir, healthFileName)))

	err := Update(dataDir)
	require.ErrorIs(t, err, ErrUnsafeHealthFile)

	content, readErr := os.ReadFile(target)
	require.NoError(t, readErr)
	assert.Equal(t, "keep me", string(content), "symlink target was modified")

	assert.ErrorIs(t, Check(dataDir), ErrUnsafeHealthFile)
	assert.ErrorIs(t, CleanUp(dataDir), ErrUnsafeHealthFile)
}

func TestHealthRejectsNonRegularFile(t *testing.T) {
	dataDir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dataDir, healthFileName), 0o700))

	assert.ErrorIs(t, Update(dataDir), ErrUnsafeHealthFile)
	assert.ErrorIs(t, Check(dataDir), ErrUnsafeHealthFile)
}

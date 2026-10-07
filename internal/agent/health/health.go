// Package health provides functions to check and update the health of the agent.
// It uses a file in the agent data dir to store the timestamp of the last successful push.
// If the timestamp is older than 90 seconds, the agent is considered unhealthy.
// NB: The agent must be started with the Start() method to be considered healthy.
//
// The file is deliberately not in a shared temp dir: on the Macs the agent runs as root and
// protected_symlinks does not exist, so a fixed name in /tmp would let any local user plant a
// symlink and have root truncate an arbitrary file.
package health

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

const healthFileName = "health"

// ErrUnsafeHealthFile is returned when the path is a symlink, not a regular file, or owned by
// another user: the agent refuses to touch it rather than follow it.
var ErrUnsafeHealthFile = errors.New("health file is unsafe (symlink, not a regular file, or owned by another user)")

func healthPath(dataDir string) string {
	return filepath.Join(dataDir, healthFileName)
}

// inspect stats the path without following symlinks and rejects anything that is not a regular
// file owned by the current user. A missing file is reported through the os error untouched.
func inspect(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || !ownedByCurrentUser(info) {
		return nil, fmt.Errorf("%w: %s", ErrUnsafeHealthFile, path)
	}
	return info, nil
}

func updateHealthFile(path string) error {
	if _, err := inspect(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	// O_NOFOLLOW closes the race between the Lstat above and the open: a symlink swapped in
	// meanwhile makes the open fail instead of truncating its target.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|noFollow, 0o600)
	if err != nil {
		return err
	}
	return file.Close()
}

// Check checks if the agent is connected by checking the modification time of the health file
func Check(dataDir string) error {
	fileInfo, err := inspect(healthPath(dataDir))
	if err != nil {
		return err
	}
	if time.Since(fileInfo.ModTime()) > 91*time.Second {
		log.Println("over 90 seconds since last connection")
		return errors.New("unhealthy")
	}
	return nil
}

// Update updates the modification time of the health file
func Update(dataDir string) error {
	return updateHealthFile(healthPath(dataDir))
}

// CleanUp removes the health file
func CleanUp(dataDir string) error {
	path := healthPath(dataDir)
	if _, err := inspect(path); err != nil {
		return err
	}
	return os.Remove(path)
}

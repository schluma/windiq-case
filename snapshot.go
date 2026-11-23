package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// snapshotFormatVersion must be raised whenever the format or the
// normalization rules change. The snapshot stores normalized values, so an
// old one would keep the old rules. An older snapshot is still loaded and
// shown, but the first poll reloads everything from the API and only then
// overwrites the snapshot. If an older format no longer decodes, the app
// starts empty.
//
// 2: slightly negative values count as calm (0 m/s).
// 3: calm down to -3 m/s, upper limit 40 m/s instead of 75 m/s.
const snapshotFormatVersion = 3

// snapshot is the on-disk copy of the store. On start it lets the app show
// data right away, even when the API is down at that moment.
type snapshot struct {
	FormatVersion int           `json:"format_version"`
	SavedAt       time.Time     `json:"saved_at"`
	Measurements  []measurement `json:"measurements"`
	Mast          *mast         `json:"mast,omitempty"`
	Turbine       *turbine      `json:"turbine,omitempty"`
	Site          *site         `json:"site,omitempty"`
}

// saveSnapshot writes the snapshot atomically: into a temporary file next to
// the target, flushed to disk, then renamed. A crash while writing therefore
// never leaves a half-written snapshot behind.
func saveSnapshot(path string, snap snapshot) error {
	dir := filepath.Dir(path)
	err := os.MkdirAll(dir, 0o755)
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".snapshot-*.tmp")
	if err != nil {
		return err
	}
	// Removes the temporary file on every error path; after the rename
	// it no longer exists and this does nothing.
	defer os.Remove(tmp.Name())

	err = json.NewEncoder(tmp).Encode(snap)
	if err != nil {
		tmp.Close()
		return err
	}
	err = tmp.Sync()
	if err != nil {
		tmp.Close()
		return err
	}
	err = tmp.Close()
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// loadSnapshot reads a snapshot. A missing file returns an error that
// matches os.ErrNotExist.
//
// outdated is true for an older format version that still decodes. Its data
// may be shown until a full reload from the API has replaced it.
func loadSnapshot(path string) (snap snapshot, outdated bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return snapshot{}, false, err
	}

	err = json.Unmarshal(data, &snap)
	if err != nil {
		return snapshot{}, false, fmt.Errorf("decode snapshot: %w", err)
	}
	if snap.FormatVersion < 1 || snap.FormatVersion > snapshotFormatVersion {
		return snapshot{}, false, fmt.Errorf("unsupported snapshot format %d", snap.FormatVersion)
	}
	return snap, snap.FormatVersion < snapshotFormatVersion, nil
}

package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/infercrane/infercrane/internal/agentprotocol"
)

// The last data plane snapshot is kept on disk, mode 0600, so an agent that
// restarts while its control plane is unreachable can serve inside its
// offline grace. It holds what the control plane sent: routes by server key,
// API key digests, admission, and the quota rate. No key and no request
// content is in it.

const dataPlaneStateFile = "data-plane-snapshot.json"

// saveInterval bounds how often a snapshot whose only change is a longer
// validity is written again.
const saveInterval = time.Minute

func (d *DataPlane) statePath() string {
	if d.agent.config.StateDir == "" {
		return ""
	}
	return filepath.Join(d.agent.config.StateDir, dataPlaneStateFile)
}

// save writes the snapshot the data plane serves with, when it changed or
// its validity moved on by more than saveInterval. A failed write is
// reported and serving goes on; only an outage then loses the snapshot.
func (d *DataPlane) save() {
	path := d.statePath()
	if path == "" {
		return
	}
	d.mu.RLock()
	snapshot, held, saved, generation := d.snapshot, d.held, d.saved, d.savedGeneration
	d.mu.RUnlock()
	if !held || snapshot.Generation == generation && snapshot.ValidUntil.Sub(saved) < saveInterval {
		return
	}
	if err := writeSnapshot(path, snapshot); err != nil {
		d.agent.emit(Event{Kind: "retrying", Message: "the data plane snapshot could not be kept on disk: " + err.Error()})
		return
	}
	d.mu.Lock()
	d.saved, d.savedGeneration = snapshot.ValidUntil, snapshot.Generation
	d.mu.Unlock()
}

func writeSnapshot(path string, snapshot agentprotocol.DataPlaneSnapshot) error {
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".data-plane-*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if err = temporary.Chmod(0o600); err == nil {
		_, err = temporary.Write(encoded)
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(temporary.Name(), path)
}

// load restores the snapshot kept on disk, if any, so the data plane can
// serve inside its offline grace before the control plane answers. A file
// that is missing, unreadable, or fails the snapshot checks is ignored.
func (d *DataPlane) load() error {
	path := d.statePath()
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var snapshot agentprotocol.DataPlaneSnapshot
	if err = json.Unmarshal(raw, &snapshot); err != nil {
		return fmt.Errorf("the kept data plane snapshot is not readable: %w", err)
	}
	if err = snapshot.ValidateStored(); err != nil || snapshot.Unchanged {
		return fmt.Errorf("the kept data plane snapshot is invalid: %v", err)
	}
	d.apply(snapshot)
	d.mu.Lock()
	d.saved, d.savedGeneration = snapshot.ValidUntil, snapshot.Generation
	d.mu.Unlock()
	return nil
}

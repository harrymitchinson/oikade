package host

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/harrymitchinson/oikade/internal/host/api"
)

const (
	// BackupsDir holds snapshots of the store, in the data directory.
	BackupsDir = "backups"
	// keptBackups bounds them. Each is the size of the store.
	keptBackups = 5
)

// BackupName is the name of a snapshot taken at a time. Names sort by age.
func BackupName(at time.Time) string {
	return "oikade-" + at.UTC().Format("20060102T150405Z") + ".db"
}

func (s *apiServer) backup() api.Response {
	dir := filepath.Join(s.dataDir, BackupsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return api.Response{Error: err.Error()}
	}
	path := filepath.Join(dir, BackupName(time.Now()))
	if err := s.data.Snapshot(path); err != nil {
		return api.Response{Error: err.Error()}
	}
	if err := PruneBackups(dir); err != nil {
		s.log.Error(err, "Failed to remove old backups")
	}
	s.log.Info("Wrote backup", "path", path)
	return api.Response{Backup: path}
}

// PruneBackups keeps the newest snapshots in dir.
func PruneBackups(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "oikade-") && strings.HasSuffix(e.Name(), ".db") {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	for len(names) > keptBackups {
		if err := os.Remove(filepath.Join(dir, names[0])); err != nil {
			return err
		}
		names = names[1:]
	}
	return nil
}

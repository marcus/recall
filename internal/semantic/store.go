package semantic

import (
	"context"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/marcus/recall/internal/semantic/antfly"
)

const databaseFile = "index.aflite"

type generation struct {
	ID            string                  `json:"id"`
	Config        string                  `json:"config"`
	Watermark     string                  `json:"watermark"`
	Built         time.Time               `json:"built"`
	Documents     map[string]indexedDoc   `json:"documents"`
	Chunks        map[string]indexedChunk `json:"chunks"`
	Failed        int                     `json:"failed,omitempty"`
	FailureSample []string                `json:"failure_sample,omitempty"`
}

type indexedDoc struct {
	Version  string   `json:"version"`
	Revision string   `json:"revision,omitempty"`
	Title    string   `json:"title,omitempty"`
	Chunks   []string `json:"chunks,omitempty"`
	Anchors  []string `json:"anchors,omitempty"`
}

type indexedChunk struct {
	Path        string `json:"path"`
	Start       int    `json:"start"`
	End         int    `json:"end"`
	Excerpt     string `json:"excerpt"`
	Fingerprint string `json:"fingerprint"`
}

type indexLock struct {
	file *os.File
}

func lockIndex(ctx context.Context, dir, name string, mode int) (indexLock, error) {
	if err := ctx.Err(); err != nil {
		return indexLock{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return indexLock{}, err
	}
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return indexLock{}, err
	}
	ticker := time.NewTicker(15 * time.Millisecond)
	defer ticker.Stop()
	for ctx.Err() == nil {
		err = syscall.Flock(int(f.Fd()), mode|syscall.LOCK_NB)
		if err == nil {
			return indexLock{f}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			_ = f.Close()
			return indexLock{}, err
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
	_ = f.Close()
	return indexLock{}, ctx.Err()
}

func (l indexLock) close() { _ = l.file.Close() }

func (a *Adapter) published() (*generation, error) {
	current, err := os.ReadFile(filepath.Join(a.dir, "current"))
	if err != nil {
		return nil, err
	}
	id := strings.TrimSpace(string(current))
	if !strings.HasPrefix(id, "gen-") || filepath.Base(id) != id {
		return nil, fmt.Errorf("invalid Antfly generation pointer")
	}
	f, err := os.Open(filepath.Join(a.dir, id, "records.json"))
	if err != nil {
		return nil, fmt.Errorf("open records for %s: %w", id, err)
	}
	defer f.Close()
	var g generation
	if err := json.NewDecoder(f).Decode(&g); err != nil {
		return nil, fmt.Errorf("read records for %s: %w", id, err)
	}
	if g.ID != id || g.Config != a.config || g.Watermark == "" || g.Built.IsZero() {
		return nil, fmt.Errorf("record metadata does not match the configured source")
	}
	if len(g.Chunks) > 0 {
		if _, err := os.Stat(filepath.Join(a.dir, id, databaseFile)); err != nil {
			return nil, fmt.Errorf("open index for %s: %w", id, err)
		}
	}
	return &g, nil
}

func (a *Adapter) cachedVectors(g *generation) (map[string][]float32, error) {
	f, err := os.Open(filepath.Join(a.dir, g.ID, "vectors.gob"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var vectors map[string][]float32
	err = gob.NewDecoder(f).Decode(&vectors)
	return vectors, err
}

func writeGeneration(dir string, g *generation, rows []antfly.Row) error {
	if err := writeSynced(filepath.Join(dir, "records.json"), func(f *os.File) error {
		return json.NewEncoder(f).Encode(g)
	}); err != nil {
		return err
	}
	vectors := make(map[string][]float32, len(rows))
	for _, row := range rows {
		vectors[row.ID] = row.Vector
	}
	if err := writeSynced(filepath.Join(dir, "vectors.gob"), func(f *os.File) error {
		return gob.NewEncoder(f).Encode(vectors)
	}); err != nil {
		return err
	}
	return syncDirectory(dir)
}

func writeSynced(file string, encode func(*os.File) error) error {
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := encode(f); err != nil {
		return err
	}
	return f.Sync()
}

func syncDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// publish changes the pointer under an exclusive lock, after the index is
// verified. Readers keep a shared lock until they finish with that generation.
func (a *Adapter) publish(ctx context.Context, g *generation) (bool, error) {
	lock, err := lockIndex(ctx, a.dir, "publish.lock", syscall.LOCK_EX)
	if err != nil {
		return false, err
	}
	defer lock.close()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	staging := filepath.Join(a.dir, "next-"+g.ID)
	defer os.Remove(staging)
	if err := writeSynced(staging, func(f *os.File) error {
		_, err := fmt.Fprintln(f, g.ID)
		return err
	}); err != nil {
		return false, err
	}
	if err := os.Rename(staging, filepath.Join(a.dir, "current")); err != nil {
		return false, err
	}
	if err := syncDirectory(a.dir); err != nil {
		return true, err
	}

	// Only one writer can build, so remaining generations have no active
	// writers. A process killed during a build is cleaned up here as well.
	entries, err := os.ReadDir(a.dir)
	if err != nil {
		return true, err
	}
	for _, entry := range entries {
		if entry.Name() == g.ID {
			continue
		}
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "gen-") ||
			!entry.IsDir() && strings.HasPrefix(entry.Name(), "next-gen-") {
			if err := os.RemoveAll(filepath.Join(a.dir, entry.Name())); err != nil {
				return true, err
			}
		}
	}
	return true, nil
}

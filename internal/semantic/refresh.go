package semantic

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/marcus/recall/internal/adapters/docs"
	"github.com/marcus/recall/internal/semantic/antfly"
	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

const embeddingBatch = 16

func (a *Adapter) Health(ctx context.Context) (recall.Health, error) {
	current, scanErr := a.scan(ctx)
	lock, err := lockIndex(ctx, a.dir, "publish.lock", syscall.LOCK_SH)
	if err != nil {
		return recall.Health{}, err
	}
	defer lock.close()
	g, loadErr := a.published()
	if loadErr == nil {
		loadErr = a.checkReadable(g)
		if loadErr != nil {
			g = nil
		}
	}
	return a.health(current, scanErr, g, loadErr), nil
}

func (a *Adapter) health(current docs.SemanticScan, scanErr error, g *generation, loadErr error) recall.Health {
	h := recall.Health{
		Status:          recall.HealthUnavailable,
		Coverage:        recall.IndexUnknown,
		CheckedAt:       time.Now().UTC(),
		SourceWatermark: current.Watermark,
		RecordCount:     int64(len(current.Files)),
		Diagnostics:     map[string]any{},
	}
	if scanErr != nil {
		h.Diagnostics["scan_error"] = scanErr.Error()
	}
	if g == nil {
		if loadErr != nil {
			h.Diagnostics["reason"] = loadErr.Error()
		}
		return h
	}

	h.IndexWatermark = g.Watermark
	h.IndexGeneration = g.ID
	h.IndexModel = a.model
	h.IndexConfig = fmt.Sprintf("%s:floor=%.4f", a.config, a.floor)
	h.LastSuccess = &g.Built
	h.Diagnostics["chunk_count"] = len(g.Chunks)
	h.FailedCount = int64(g.Failed)
	if len(g.FailureSample) > 0 {
		h.Diagnostics["failure_sample"] = g.FailureSample
	}
	for _, file := range current.Files {
		if doc, ok := g.Documents[file.Path]; ok && doc.Version == file.Version {
			if len(doc.Chunks) > 0 {
				h.IndexedCount++
			} else {
				h.RecordCount-- // empty and noindex files are outside retrieval
			}
		}
	}
	stale := scanErr != nil || current.Watermark != g.Watermark
	h.Diagnostics["stale"] = stale
	if stale || g.Failed > 0 {
		h.Status, h.Coverage = recall.HealthDegraded, recall.IndexPartial
	} else {
		h.Status, h.Coverage = recall.HealthHealthy, recall.IndexComplete
	}
	return h
}

func (a *Adapter) Refresh(ctx context.Context, params protocol.RefreshParams) (recall.Health, error) {
	ctx, cancel := withDeadline(ctx, params.Deadline)
	defer cancel()
	writer, err := lockIndex(ctx, a.dir, "refresh.lock", syscall.LOCK_EX)
	if err != nil {
		return recall.Health{}, err
	}
	defer writer.close()

	current, err := a.scan(ctx)
	old, loadErr := a.published()
	if ctx.Err() != nil {
		return recall.Health{}, ctx.Err()
	}
	if err != nil {
		return a.health(current, err, old, loadErr), nil
	}
	if !params.Full && old != nil && old.Watermark == current.Watermark && old.Failed == 0 && a.checkReadable(old) == nil {
		h := a.health(current, nil, old, nil)
		h.CheckpointProgress = recall.CheckpointUnchanged
		return h, nil
	}

	g, err := a.rebuild(ctx, current, old, params.Full)
	if ctx.Err() != nil && g == nil {
		return recall.Health{}, ctx.Err()
	}
	if err != nil {
		if g == nil && old != nil {
			if readErr := a.checkReadable(old); readErr != nil {
				loadErr = readErr
			} else {
				g = old
			}
		}
		var scanErr error
		if errors.Is(err, errCorpusMoved) {
			current, scanErr = a.scan(ctx)
		}
		h := a.health(current, scanErr, g, loadErr)
		if g != nil {
			h.Status = recall.HealthDegraded
		}
		h.Diagnostics["last_refresh_error"] = err.Error()
		return h, nil
	}

	h := a.health(current, nil, g, nil)
	h.CheckpointProgress = recall.CheckpointAdvanced
	if old != nil && old.Watermark == g.Watermark {
		h.CheckpointProgress = recall.CheckpointUnchanged
	}
	return h, nil
}

var errCorpusMoved = errors.New("corpus changed during refresh; retry against the new boundary")

func (a *Adapter) rebuild(ctx context.Context, current docs.SemanticScan, old *generation, full bool) (*generation, error) {
	var cached map[string][]float32
	if old != nil && !full {
		// Losing the rebuild cache costs embeddings. The serving index stays valid.
		cached, _ = a.cachedVectors(old)
	}
	g := &generation{
		Config:    a.config,
		Watermark: current.Watermark,
		Documents: make(map[string]indexedDoc, len(current.Files)),
		Chunks:    map[string]indexedChunk{},
	}
	rows, err := a.collect(ctx, g, current.Files, old, cached)
	if err != nil {
		return nil, err
	}
	staged, err := os.MkdirTemp(a.dir, "gen-")
	if err != nil {
		return nil, err
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(staged)
		}
	}()
	g.ID = filepath.Base(staged)

	if len(rows) > 0 {
		file := filepath.Join(staged, databaseFile)
		if err := a.build(ctx, file, a.dimensions, rows); err != nil {
			return nil, fmt.Errorf("build Antfly index: %w", err)
		}
		if err := a.verify(ctx, file, rows); err != nil {
			return nil, fmt.Errorf("reopen Antfly index: %w", err)
		}
	}
	g.Built = time.Now().UTC()
	if err := writeGeneration(staged, g, rows); err != nil {
		return nil, err
	}
	latest, err := a.scan(ctx)
	if err != nil {
		return nil, err
	}
	if latest.Watermark != current.Watermark {
		return nil, errCorpusMoved
	}
	published, err = a.publish(ctx, g)
	if !published {
		return nil, err
	}
	return g, err
}

func (a *Adapter) collect(ctx context.Context, g *generation, files []docs.SemanticFile, old *generation, cached map[string][]float32) ([]antfly.Row, error) {
	var rows []antfly.Row
	var pending []string
	var texts, titles []string
	flush := func() error {
		if len(texts) == 0 {
			return nil
		}
		vectors, err := a.embed.EmbedDocuments(ctx, texts, titles)
		if err != nil {
			return fmt.Errorf("embed documents: %w", err)
		}
		if len(vectors) != len(pending) {
			return errors.New("embedding batch returned a different number of vectors")
		}
		for i, id := range pending {
			rows = append(rows, antfly.Row{ID: id, Vector: vectors[i]})
		}
		pending, texts, titles = pending[:0], texts[:0], titles[:0]
		return nil
	}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if doc, ok := fromCache(old, cached, file, a.dimensions); ok {
			for _, id := range doc.Chunks {
				g.Chunks[id] = old.Chunks[id]
				rows = append(rows, antfly.Row{ID: id, Vector: cached[id]})
			}
			g.Documents[file.Path] = doc
			continue
		}
		doc, err := a.read(file.Path)
		if err != nil {
			g.Failed++
			if len(g.FailureSample) < 5 {
				g.FailureSample = append(g.FailureSample, file.Path+": "+err.Error())
			}
			continue
		}
		meta := indexedDoc{
			Version: file.Version, Revision: fileRevision(doc.Hash),
			Title: bounded(doc.Title, 240),
		}
		anchors := map[string]struct{}{}
		addAnchors(anchors, file.Path)
		for _, chunk := range doc.Chunks {
			if strings.TrimSpace(chunk.Text) == "" {
				continue
			}
			id := fmt.Sprintf("%s#%d", file.Path, len(meta.Chunks))
			meta.Chunks = append(meta.Chunks, id)
			addAnchors(anchors, chunk.Text)
			g.Chunks[id] = indexedChunk{
				Path: file.Path, Start: chunk.Start, End: chunk.End,
				Excerpt: bounded(chunk.Text, 480), Fingerprint: chunk.Fingerprint,
			}
			pending = append(pending, id)
			texts = append(texts, chunk.Text)
			titles = append(titles, meta.Title)
			if len(texts) == embeddingBatch {
				if err := flush(); err != nil {
					return nil, err
				}
			}
		}
		meta.Anchors = slices.Sorted(maps.Keys(anchors))
		g.Documents[file.Path] = meta
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return rows, nil
}

func fromCache(g *generation, cached map[string][]float32, file docs.SemanticFile, dimensions int) (indexedDoc, bool) {
	if g == nil || cached == nil {
		return indexedDoc{}, false
	}
	doc, ok := g.Documents[file.Path]
	if !ok || doc.Version != file.Version {
		return indexedDoc{}, false
	}
	for _, id := range doc.Chunks {
		if len(cached[id]) != dimensions {
			return indexedDoc{}, false
		}
	}
	return doc, true
}

// checkReadable distinguishes files present on disk from an openable Antfly
// generation. Refresh can reconstruct an unreadable database from cached vectors.
func (a *Adapter) checkReadable(g *generation) error {
	if len(g.Chunks) == 0 {
		return nil
	}
	idx, err := a.open(filepath.Join(a.dir, g.ID, databaseFile))
	if err != nil {
		return fmt.Errorf("open published Antfly index: %w", err)
	}
	return idx.Close()
}

func (a *Adapter) verify(ctx context.Context, file string, rows []antfly.Row) error {
	index, err := a.open(file)
	if err != nil {
		return err
	}
	defer func() { _ = index.Close() }()
	for _, i := range []int{0, len(rows) / 2, len(rows) - 1} {
		hits, err := index.Search(ctx, rows[i].Vector, min(10, len(rows)))
		if err != nil {
			return err
		}
		// Identical chunks can tie. The score remains a useful assertion even
		// when ten identical peers push the probe's original ID off the page.
		if len(hits) == 0 || hits[0].Score < 0.99 {
			return errors.New("exact validation did not find a matching vector")
		}
	}
	return nil
}

func fileRevision(hash string) string {
	if hash == "" {
		return ""
	}
	return "file:" + hash[:min(len(hash), 12)]
}

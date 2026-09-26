// Package inbox hands the agent's draft files to the bot until the two talk
// through Kafka: the bot picks up the files the agent writes into a
// directory, stores the drafts and tells the admins. Only for local runs,
// where both work on one machine.
package inbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/KKolyasik/max-benefits/internal/agent"
	"github.com/KKolyasik/max-benefits/internal/moderation"
)

// Sink stores drafts.
type Sink interface {
	AddDraft(ctx context.Context, externalID string, d moderation.Draft) (bool, error)
}

// Import stores every draft file of the directory and removes the imported
// files. A file that can't be read yet, e.g. one the agent is still writing,
// is left for the next time. It returns how many drafts are new.
func Import(ctx context.Context, dir string, sink Sink, log *slog.Logger) (int, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return 0, err
	}
	slices.Sort(paths)
	files := agent.Drafts{Dir: dir}
	added := 0
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			log.Warn("read draft file", "path", path, "err", err)
			continue
		}
		d, err := files.Load(path)
		if err != nil {
			log.Warn("draft file is not valid yet", "path", path, "err", err)
			continue
		}
		// The same file content is the same draft, whatever its name.
		sum := sha256.Sum256(data)
		isNew, err := sink.AddDraft(ctx, "file:"+hex.EncodeToString(sum[:]), moderation.Draft{
			Card: d.Card, Updates: d.Updates, Query: d.Query, Sources: d.Sources, Notes: d.Notes, FoundAt: d.FoundAt,
		})
		if err != nil {
			return added, err
		}
		if isNew {
			added++
		}
		if err := files.Delete(d); err != nil {
			return added, err
		}
	}
	return added, nil
}

// Watch imports the directory every interval until ctx is done and passes
// the number of new drafts to notify.
func Watch(ctx context.Context, dir string, every time.Duration, sink Sink,
	notify func(context.Context, int) error, log *slog.Logger,
) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		added, err := Import(ctx, dir, sink, log)
		if err != nil {
			log.Error("import drafts", "dir", dir, "err", err)
		}
		if added > 0 {
			log.Info("new drafts", "count", added)
			if err := notify(ctx, added); err != nil {
				log.Error("notify admins", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

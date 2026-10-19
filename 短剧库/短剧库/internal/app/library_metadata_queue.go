package app

import (
	"context"
	"time"
)

func (a *UIApp) newSortMetadataDramasLocked(items []Drama) []Drama {
	known := make(map[string]bool, a.libraryLen())
	for _, entry := range a.libraryEntries() {
		known[entry.ID] = true
	}
	var fresh []Drama
	for _, drama := range items {
		if drama.ID != "" && !known[drama.ID] {
			known[drama.ID] = true
			if needsSortMetadata(drama) {
				fresh = append(fresh, drama)
			}
		}
	}
	return fresh
}

func (a *UIApp) enqueueHistoricalMetadataLocked(source string, priority []string) {
	var available []Drama
	for _, drama := range a.libraryIndexDramas() {
		if !a.metadataPending[drama.ID] {
			available = append(available, drama)
		}
	}
	a.enqueueSortMetadataLocked(selectSortMetadataBatch(available, source, priority, time.Now()), true)
}

func (a *UIApp) enqueueSortMetadataLocked(items []Drama, prioritize bool) {
	if a.metadataClosed || len(items) == 0 {
		return
	}
	if a.metadataPending == nil {
		a.metadataPending = map[string]bool{}
	}
	var added []string
	for _, drama := range items {
		if drama.ID == "" || !needsSortMetadata(drama) || a.metadataPending[drama.ID] {
			continue
		}
		a.metadataPending[drama.ID] = true
		added = append(added, drama.ID)
	}
	if len(added) == 0 {
		return
	}

	a.libraryUpdateEntries(added, func(entry *dramaEntry) bool {
		if !a.metadataPending[entry.ID] || entry.SortMetadata != nil && entry.SortMetadata.Pending {
			return false
		}
		state := sortMetadataState{Pending: true}
		if entry.SortMetadata != nil {
			state = *entry.SortMetadata
			state.Pending = true
		}
		entry.SortMetadata = &state
		return true
	})
	if prioritize {
		a.metadataQueue = append(added, a.metadataQueue...)
	} else {
		a.metadataQueue = append(a.metadataQueue, added...)
	}
	if !a.libraryMetadata.Running {
		a.libraryMetadata = libraryMetadataProgress{Running: true}
	}
	a.libraryMetadata.Total += len(added)
	a.libraryDirty = true
	a.libraryRevision++
	if a.metadataDone == nil {
		ctx, cancel := context.WithCancel(context.Background())
		a.metadataCancel = cancel
		a.metadataDone = make(chan struct{})
		go a.runSortMetadataQueue(ctx, a.metadataDone)
	}
}

func (a *UIApp) runSortMetadataQueue(ctx context.Context, done chan struct{}) {

	ctx = context.WithValue(ctx, backgroundCatalogKey{}, true)
	for {
		a.mu.Lock()
		if ctx.Err() != nil || len(a.metadataQueue) == 0 {
			a.mu.Unlock()
			a.persistLibrary()
			a.mu.Lock()

			if ctx.Err() == nil && len(a.metadataQueue) > 0 {
				a.mu.Unlock()
				continue
			}
			a.libraryMetadata.Running = false
			a.libraryRevision++
			a.metadataCancel()
			a.metadataCancel = nil
			a.metadataDone = nil
			a.metadataQueue = nil
			close(done)
			a.mu.Unlock()
			return
		}
		if a.libraryLoading != nil {
			a.mu.Unlock()
			timer := time.NewTimer(500 * time.Millisecond)
			select {
			case <-ctx.Done():
			case <-timer.C:
			}
			timer.Stop()
			continue
		}
		id := a.metadataQueue[0]
		a.metadataQueue = a.metadataQueue[1:]
		previous, _ := a.libraryDrama(id)
		a.mu.Unlock()
		itemCtx, cancel := context.WithTimeout(ctx, 40*time.Second)
		patch, err := a.downloader.fetchDramaSortMetadata(itemCtx, previous)
		cancel()
		if err != nil && ctx.Err() == nil {
			a.downloader.recordDiagnostic(diagnosticEvent{Event: "metadata.failed", Source: dramaProvider(previous), DramaID: previous.ID, DramaTitle: previous.DisplayTitle(), Message: err.Error()})
		}
		now := time.Now()
		patch.SortMetadata = &sortMetadataState{CheckedAt: now}
		if err == nil {
			patch.SortMetadata.Version = dramaSortMetadataVersion(previous)
			patch.SortMetadata.CoverChecked = dramaProvider(previous) == sourceHongguo
			patch.SortMetadata.VIPChecked = dramaProvider(previous) == sourceHuangdou
		} else {
			patch.SortMetadata = failedSortMetadataState(previous, err, now)
		}
		normalizeDramaCover(&patch)
		a.mu.Lock()
		if ctx.Err() != nil {

			a.mu.Unlock()
			continue
		}
		delete(a.metadataPending, id)
		a.libraryUpdate(id, func(drama *Drama) bool {
			_, sourceID, _ := splitProviderDramaID(drama.ID)
			if dramaProvider(*drama) != sourceHuangguoAI || firstHuangguoTitle(sourceID, drama.Title, drama.Name) != "" {
				patch.Title, patch.Name = drama.Title, drama.Name
			}
			previous := *drama
			updated := mergeDramaMetadata(patch, previous)
			if patch.VIP != nil && (previous.VIP == nil || *patch.VIP != *previous.VIP) || patch.OnlineDate != "" && patch.OnlineDate != previous.OnlineDate || patch.Heat != "" && patch.Heat != previous.Heat || patch.Views != "" && patch.Views != previous.Views || bestDramaCover(patch) != "" && bestDramaCover(patch) != bestDramaCover(previous) || huangguoContentChanged(previous, updated) {
				a.libraryMetadata.Updated++
			}
			*drama = updated
			return true
		})
		a.libraryMetadata.Checked++
		if err != nil {
			a.libraryMetadata.Failed++
		}
		a.libraryDirty = true
		a.libraryRevision++
		persist := time.Since(a.libraryLastSave) >= 3*time.Second
		if persist {
			a.libraryLastSave = time.Now()
		}
		a.mu.Unlock()
		if persist {
			a.persistLibrary()
		}
	}
}

func (a *UIApp) stopSortMetadata() {
	a.mu.Lock()
	a.metadataClosed = true
	cancel, done := a.metadataCancel, a.metadataDone
	a.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

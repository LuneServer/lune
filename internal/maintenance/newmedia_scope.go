package maintenance

import "context"

// newMediaScope limits a new-media pass to the items a library scan just added or changed,
// and relaxes when that pass yields: it keeps running through direct play (the server only
// serves file bytes, so the niced, single-threaded FFmpeg job barely competes) and pauses
// only for scans and FFmpeg playback sessions. Backlog passes keep yielding to any playback.
type newMediaScope struct {
	itemIDs map[string]struct{}
}

type newMediaScopeKey struct{}

func withNewMediaScope(ctx context.Context, itemIDs map[string]struct{}) context.Context {
	return context.WithValue(ctx, newMediaScopeKey{}, &newMediaScope{itemIDs: itemIDs})
}

func newMediaScopeFrom(ctx context.Context) *newMediaScope {
	sc, _ := ctx.Value(newMediaScopeKey{}).(*newMediaScope)
	return sc
}

// includes reports whether a per-item task should process itemID in this run: always for
// backlog passes, and only for the scan's new or changed items in a new-media pass.
func (sc *newMediaScope) includes(itemID string) bool {
	if sc == nil {
		return true
	}
	_, ok := sc.itemIDs[itemID]
	return ok
}

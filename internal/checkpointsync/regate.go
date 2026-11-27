package checkpointsync

import (
	"slices"

	"github.com/infercrane/infercrane/internal/checkpointcatalog"
)

// RegateChange is one entry whose eligibility moved when the gates were
// applied again.
type RegateChange struct {
	ID     string
	Before checkpointcatalog.Eligibility
	After  checkpointcatalog.Eligibility
}

// Regate applies the eligibility gates again to a committed catalog, against
// the runtime images pinned now. Moving an engine pin changes which builds a
// pinned image can serve, and nothing else an entry records depends on the
// pin, so this needs no new snapshot and leaves every reviewed field as it
// is. It returns the regated catalog and the entries whose eligibility moved.
func Regate(catalog checkpointcatalog.Catalog) (checkpointcatalog.Catalog, []RegateChange) {
	out := catalog
	out.Entries = slices.Clone(catalog.Entries)
	var changes []RegateChange
	for index := range out.Entries {
		before := out.Entries[index].Eligibility
		after := eligibility(out.Entries[index])
		if before.State == after.State && slices.Equal(before.Blockers, after.Blockers) {
			continue
		}
		out.Entries[index].Eligibility = after
		changes = append(changes, RegateChange{ID: out.Entries[index].ID, Before: before, After: after})
	}
	return out, changes
}

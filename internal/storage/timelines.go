package storage

import (
	"errors"
	"os"

	"voice-timeline-renderer/internal/domain"
)

func (repo *Repository) SaveTimeline(plan domain.Timeline) error {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	path := repo.path("timelines", plan.ID+".json")
	if _, err := os.Stat(path); err == nil {
		return domain.E(domain.KindConflict, "timeline_conflict", "timeline_id", "timeline identifier already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot inspect timeline destination", err)
	}
	if err := writeJSONAtomic(path, plan, 0o640); err != nil {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot write timeline", err)
	}
	return nil
}

func (repo *Repository) GetTimeline(id string) (domain.Timeline, error) {
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	var plan domain.Timeline
	if err := readJSON(repo.path("timelines", id+".json"), &plan); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return domain.Timeline{}, domain.E(domain.KindNotFound, "timeline_not_found", "timeline_id", "timeline does not exist")
		}
		return domain.Timeline{}, domain.Wrap(domain.KindInternal, "storage_error", "", "cannot read timeline", err)
	}
	return plan, nil
}

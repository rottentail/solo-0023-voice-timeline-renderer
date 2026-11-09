package storage

import (
	"os"
	"path/filepath"
	"sync"
)

type Repository struct {
	root string
	mu   sync.RWMutex
}

func NewRepository(root string) (*Repository, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	for _, child := range []string{"sources", "timelines", "jobs", "renders", "tmp"} {
		if err := os.MkdirAll(filepath.Join(absolute, child), 0o750); err != nil {
			return nil, err
		}
	}
	return &Repository{root: absolute}, nil
}

func (repo *Repository) path(parts ...string) string {
	all := append([]string{repo.root}, parts...)
	return filepath.Join(all...)
}

package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

var (
	errNotFound = errors.New("site not found")
)

type Site struct {
	ID            int64      `json:"id"`
	URL           string     `json:"url"`
	Status        string     `json:"status"`
	StatusCode    int        `json:"statusCode,omitempty"`
	LastCheckedAt *time.Time `json:"lastCheckedAt,omitempty"`
	LastError     string     `json:"lastError,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
	RequestedAt   time.Time  `json:"-"`
}

type siteStore interface {
	Close() error
	Create(context.Context, string) (Site, error)
	List(context.Context) ([]Site, error)
	Due(context.Context, time.Time) ([]Site, error)
	Evict(context.Context, time.Time) error
	SaveCheck(context.Context, int64, string, int, time.Time, string) error
	Health(context.Context) error
}

type memoryStore struct {
	mu    sync.RWMutex
	next  int64
	sites map[int64]Site
	byURL map[string]int64
}

func newMemoryStore() *memoryStore {
	return &memoryStore{
		sites: make(map[int64]Site),
		byURL: make(map[string]int64),
	}
}

func (s *memoryStore) Close() error {
	return nil
}

func (s *memoryStore) Health(ctx context.Context) error {
	return ctx.Err()
}

func (s *memoryStore) Create(ctx context.Context, target string) (Site, error) {
	if err := ctx.Err(); err != nil {
		return Site{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	requestedAt := time.Now().UTC()
	if id, exists := s.byURL[target]; exists {
		site := s.sites[id]
		site.RequestedAt = requestedAt
		s.sites[id] = site
		return site, nil
	}

	s.next++
	site := Site{
		ID:          s.next,
		URL:         target,
		Status:      "pending",
		CreatedAt:   requestedAt,
		RequestedAt: requestedAt,
	}
	s.sites[site.ID] = site
	s.byURL[target] = site.ID
	return site, nil
}

func (s *memoryStore) List(ctx context.Context) ([]Site, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	sites := make([]Site, 0, len(s.sites))
	for _, site := range s.sites {
		sites = append(sites, site)
	}
	sort.Slice(sites, func(i, j int) bool {
		return sites[i].ID > sites[j].ID
	})
	return sites, nil
}

func (s *memoryStore) Evict(ctx context.Context, before time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for id, site := range s.sites {
		if !site.RequestedAt.After(before) {
			delete(s.sites, id)
			delete(s.byURL, site.URL)
		}
	}
	return nil
}

func (s *memoryStore) Due(ctx context.Context, before time.Time) ([]Site, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	var sites []Site
	for _, site := range s.sites {
		if site.LastCheckedAt == nil || !site.LastCheckedAt.After(before) {
			sites = append(sites, site)
		}
	}
	sort.Slice(sites, func(i, j int) bool {
		return sites[i].ID < sites[j].ID
	})
	return sites, nil
}

func (s *memoryStore) SaveCheck(ctx context.Context, id int64, status string, statusCode int, checkedAt time.Time, lastError string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	site, exists := s.sites[id]
	if !exists {
		return errNotFound
	}
	site.Status = status
	site.StatusCode = statusCode
	site.LastCheckedAt = &checkedAt
	site.LastError = lastError
	s.sites[id] = site
	return nil
}

type postgresStore struct {
	db *sql.DB
}

func openStore(ctx context.Context, databaseURL string) (siteStore, string, error) {
	if strings.TrimSpace(databaseURL) == "" {
		return newMemoryStore(), "in-memory", nil
	}
	store, err := openPostgres(ctx, databaseURL)
	if err != nil {
		return nil, "", err
	}
	return store, "PostgreSQL", nil
}

func openPostgres(ctx context.Context, databaseURL string) (*postgresStore, error) {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL: %w", err)
	}
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	for _, statement := range schemaStatements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			db.Close()
			return nil, fmt.Errorf("prepare PostgreSQL schema: %w", err)
		}
	}
	return &postgresStore{db: db}, nil
}

var schemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS sites (
		id BIGSERIAL PRIMARY KEY,
		url TEXT NOT NULL UNIQUE,
		status TEXT NOT NULL DEFAULT 'pending',
		status_code INTEGER,
		last_checked_at TIMESTAMPTZ,
		last_error TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		requested_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`,
	`ALTER TABLE sites ADD COLUMN IF NOT EXISTS requested_at TIMESTAMPTZ`,
	`UPDATE sites SET requested_at = created_at WHERE requested_at IS NULL`,
	`ALTER TABLE sites ALTER COLUMN requested_at SET DEFAULT NOW()`,
	`ALTER TABLE sites ALTER COLUMN requested_at SET NOT NULL`,
	`CREATE INDEX IF NOT EXISTS sites_last_checked_at_idx ON sites (last_checked_at)`,
	`CREATE INDEX IF NOT EXISTS sites_requested_at_idx ON sites (requested_at)`,
}

const siteColumns = `id, url, status, status_code, last_checked_at, last_error, created_at, requested_at`

func (s *postgresStore) Close() error {
	return s.db.Close()
}

func (s *postgresStore) Health(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func (s *postgresStore) Create(ctx context.Context, target string) (Site, error) {
	row := s.db.QueryRowContext(ctx, `
		INSERT INTO sites (url) VALUES ($1)
		ON CONFLICT (url) DO UPDATE SET requested_at = NOW()
		RETURNING `+siteColumns, target)
	site, err := scanSite(row)
	if err != nil {
		return Site{}, err
	}
	return site, nil
}

func (s *postgresStore) List(ctx context.Context) ([]Site, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+siteColumns+` FROM sites ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	sites := make([]Site, 0)
	for rows.Next() {
		site, err := scanSite(rows)
		if err != nil {
			return nil, err
		}
		sites = append(sites, site)
	}
	return sites, rows.Err()
}

func (s *postgresStore) Evict(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sites WHERE requested_at <= $1`, before)
	return err
}

func (s *postgresStore) Due(ctx context.Context, before time.Time) ([]Site, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+siteColumns+`
		FROM sites
		WHERE last_checked_at IS NULL OR last_checked_at <= $1
		ORDER BY last_checked_at NULLS FIRST, id ASC`, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	sites := make([]Site, 0)
	for rows.Next() {
		site, err := scanSite(rows)
		if err != nil {
			return nil, err
		}
		sites = append(sites, site)
	}
	return sites, rows.Err()
}

func (s *postgresStore) SaveCheck(ctx context.Context, id int64, status string, statusCode int, checkedAt time.Time, lastError string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE sites
		SET status = $2, status_code = $3, last_checked_at = $4, last_error = $5
		WHERE id = $1`, id, status, statusCode, checkedAt, lastError)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return errNotFound
	}
	return nil
}

func scanSite(row interface{ Scan(...any) error }) (Site, error) {
	var site Site
	var checkedAt sql.NullTime
	var statusCode sql.NullInt64
	if err := row.Scan(
		&site.ID,
		&site.URL,
		&site.Status,
		&statusCode,
		&checkedAt,
		&site.LastError,
		&site.CreatedAt,
		&site.RequestedAt,
	); err != nil {
		return Site{}, err
	}
	if statusCode.Valid {
		site.StatusCode = int(statusCode.Int64)
	}
	if checkedAt.Valid {
		time := checkedAt.Time.UTC()
		site.LastCheckedAt = &time
	}
	site.CreatedAt = site.CreatedAt.UTC()
	site.RequestedAt = site.RequestedAt.UTC()
	return site, nil
}

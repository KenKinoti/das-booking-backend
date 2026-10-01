// Package dbstatus records database start-up health (migration errors, pooler
// problems) so /health and the platform admin can show it.
package dbstatus

import "sync"

var (
	mu        sync.RWMutex
	errs      []string
	pooler    string
	migrated  bool
	migrating bool
	skipped   bool
	rewritten string
)

// SetMigrating marks migrations as running (clears earlier errors).
func SetMigrating() { mu.Lock(); migrating = true; migrated = false; errs = nil; mu.Unlock() }

// SetSkipped records that migrations were skipped because this build already migrated.
func SetSkipped(v bool) { mu.Lock(); skipped = v; mu.Unlock() }

// Ready reports whether the schema is up to date for this build.
func Ready() bool { mu.RLock(); defer mu.RUnlock(); return migrated }

// AddMigrationError records a failed migration step.
func AddMigrationError(msg string) {
	mu.Lock()
	defer mu.Unlock()
	if len(errs) < 50 {
		errs = append(errs, msg)
	}
}

// SetMigrated marks migrations as finished.
func SetMigrated() { mu.Lock(); migrated = true; migrating = false; mu.Unlock() }

// SetPoolerWarning records that the connection looks like a transaction pooler.
func SetPoolerWarning(msg string) { mu.Lock(); pooler = msg; mu.Unlock() }

// SetRewrite records an automatic DATABASE_URL adjustment.
func SetRewrite(msg string) { mu.Lock(); rewritten = msg; mu.Unlock() }

// Snapshot is the public summary.
type Snapshot struct {
	MigrationsDone   bool     `json:"migrations_done"`
	Migrating        bool     `json:"migrating"`
	SkippedSameBuild bool     `json:"skipped_same_build,omitempty"`
	MigrationErrors  int      `json:"migration_errors"`
	Errors           []string `json:"errors,omitempty"`
	PoolerWarning    string   `json:"pooler_warning,omitempty"`
	ConnectionNote   string   `json:"connection_note,omitempty"`
}

// Get returns the current status. withDetail includes the error messages.
func Get(withDetail bool) Snapshot {
	mu.RLock()
	defer mu.RUnlock()
	s := Snapshot{MigrationsDone: migrated, Migrating: migrating, SkippedSameBuild: skipped, MigrationErrors: len(errs), PoolerWarning: pooler, ConnectionNote: rewritten}
	if withDetail {
		s.Errors = append([]string(nil), errs...)
	}
	return s
}

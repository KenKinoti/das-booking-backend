package platform

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

type tableStat struct {
	Name         string     `json:"name"`
	Rows         int64      `json:"rows"`
	DeadRows     int64      `json:"dead_rows"`
	TotalBytes   int64      `json:"total_bytes"`
	TableBytes   int64      `json:"table_bytes"`
	IndexBytes   int64      `json:"index_bytes"`
	SeqScans     int64      `json:"seq_scans"`
	IdxScans     int64      `json:"idx_scans"`
	Inserts      int64      `json:"inserts"`
	Updates      int64      `json:"updates"`
	Deletes      int64      `json:"deletes"`
	LastAnalyzed *time.Time `json:"last_analyzed"`
	LastVacuumed *time.Time `json:"last_vacuumed"`
}

// DatabaseHealth reports real PostgreSQL statistics for the app's schema.
func (h *Handler) DatabaseHealth(c *gin.Context) {
	db := h.DB
	var info struct {
		Version     string
		Database    string
		Schema      string
		SizeBytes   int64
		Connections int64
		MaxConn     string
		StartedAt   *time.Time
	}
	if err := db.Raw(`SELECT version() AS version, current_database() AS database, current_schema() AS schema,
		pg_database_size(current_database()) AS size_bytes,
		(SELECT COUNT(*) FROM pg_stat_activity WHERE datname = current_database()) AS connections,
		current_setting('max_connections') AS max_conn,
		pg_postmaster_start_time() AS started_at`).Scan(&info).Error; err != nil {
		fail(c, http.StatusInternalServerError, "DATABASE_ERROR", "Could not read database statistics")
		return
	}

	tables := []tableStat{}
	db.Raw(`SELECT s.relname AS name, s.n_live_tup AS rows, s.n_dead_tup AS dead_rows,
			pg_total_relation_size(s.relid) AS total_bytes, pg_relation_size(s.relid) AS table_bytes,
			pg_indexes_size(s.relid) AS index_bytes, COALESCE(s.seq_scan,0) AS seq_scans, COALESCE(s.idx_scan,0) AS idx_scans,
			s.n_tup_ins AS inserts, s.n_tup_upd AS updates, s.n_tup_del AS deletes,
			GREATEST(s.last_analyze, s.last_autoanalyze) AS last_analyzed,
			GREATEST(s.last_vacuum, s.last_autovacuum) AS last_vacuumed
		FROM pg_stat_user_tables s WHERE s.schemaname = current_schema()
		ORDER BY pg_total_relation_size(s.relid) DESC`).Scan(&tables)

	var totalRows, dead int64
	for _, t := range tables {
		totalRows += t.Rows
		dead += t.DeadRows
	}

	var cache struct{ Ratio *float64 }
	db.Raw(`SELECT CASE WHEN SUM(heap_blks_hit) + SUM(heap_blks_read) = 0 THEN NULL
		ELSE SUM(heap_blks_hit)::float / (SUM(heap_blks_hit) + SUM(heap_blks_read)) END AS ratio
		FROM pg_statio_user_tables WHERE schemaname = current_schema()`).Scan(&cache)

	sqlDB, _ := db.DB()
	pool := gin.H{}
	if sqlDB != nil {
		st := sqlDB.Stats()
		pool = gin.H{"open": st.OpenConnections, "in_use": st.InUse, "idle": st.Idle, "max_open": st.MaxOpenConnections, "wait_count": st.WaitCount}
	}

	ok(c, http.StatusOK, gin.H{
		"server": gin.H{
			"version": info.Version, "database": info.Database, "schema": info.Schema,
			"size_bytes": info.SizeBytes, "connections": info.Connections, "max_connections": info.MaxConn,
			"started_at": info.StartedAt, "cache_hit_ratio": cache.Ratio,
		},
		"pool":        pool,
		"tables":      tables,
		"table_count": len(tables),
		"total_rows":  totalRows,
		"dead_rows":   dead,
		"checked_at":  time.Now(),
	})
}

// AnalyzeDatabase refreshes planner statistics for the app's schema. ANALYZE
// is read-only for data, safe to run online and works on managed Postgres.
func (h *Handler) AnalyzeDatabase(c *gin.Context) {
	var tables []string
	h.DB.Raw(`SELECT relname FROM pg_stat_user_tables WHERE schemaname = current_schema()`).Scan(&tables)
	start := time.Now()
	failed := 0
	for _, t := range tables {
		if err := h.DB.Exec(`ANALYZE "` + escapeIdent(t) + `"`).Error; err != nil {
			failed++
		}
	}
	dur := time.Since(start)
	Audit(h.DB, c, "", "database.analyze", "database", "analyze", nil, gin.H{"tables": len(tables), "failed": failed, "ms": dur.Milliseconds()})
	ok(c, http.StatusOK, gin.H{"tables": len(tables), "failed": failed, "duration_ms": dur.Milliseconds()})
}

func escapeIdent(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '"' {
			out = append(out, '"')
		}
		out = append(out, r)
	}
	return string(out)
}
